package bitwarden

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/secret"
)

const (
	vaultEmail    = "user@example.com"
	vaultPassword = "master-password"
)

type reply struct {
	status int
	body   string
}

// vault is a fake Bitwarden server. Each endpoint answers with its reply; syncSeq, when set, is consumed first, one reply per sync.
type vault struct {
	t       *testing.T
	srv     *httptest.Server
	userKey symmetricKey
	orgKey  symmetricKey

	mu                             sync.Mutex
	prelogin, token, refresh, sync reply
	syncSeq                        []reply
	gate                           chan struct{} // when set, prelogin blocks until it is closed
	calls                          []string      // "METHOD path" or "POST …token grant_type", then what authenticated it
	loginForms                     []url.Values
	preloginBodies                 []string
}

// enc seals a string under the user key, as the vault stores it.
func (v *vault) enc(s string) string { return encryptType2String(v.t, v.userKey, s) }

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newVault(t *testing.T) *vault {
	t.Helper()
	v := &vault{t: t, userKey: symKey(1), orgKey: symKey(100)}
	master, err := makeMasterKey(vaultPassword, vaultEmail, kdfPBKDF2, 1000, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stretched, err := stretchKey(master)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(testRSAKey())
	if err != nil {
		t.Fatal(err)
	}
	org := func(s string) string { return encryptType2String(t, v.orgKey, s) }

	v.prelogin = reply{200, `{"kdf":0,"kdfIterations":1000}`}
	v.token = reply{200, jsonOf(t, map[string]any{
		"access_token": "access-token", "expires_in": 3600, "token_type": "Bearer", "refresh_token": "refresh-token",
		"Key": encryptType2(t, stretched, append(bytes.Clone(v.userKey.encKey), v.userKey.macKey...)),
	})}
	v.refresh = reply{200, `{"access_token":"refreshed-token","expires_in":3600,"refresh_token":"refresh-token-2"}`}
	v.sync = reply{200, jsonOf(t, map[string]any{
		"profile": map[string]any{
			"email": vaultEmail, "privateKey": encryptType2(t, v.userKey, privDER),
			"organizations": []any{map[string]any{"id": "org-abc", "key": rsaEncrypt(t, "4", sha1.New(), append(bytes.Clone(v.orgKey.encKey), v.orgKey.macKey...))}},
		},
		"ciphers": []any{
			map[string]any{
				"id": "cipher-personal", "type": 1, "name": v.enc("openai-key"), "notes": v.enc("rotate monthly"),
				"login": map[string]any{"username": v.enc("api-user"), "password": v.enc("sk-secret-value"), "uri": v.enc("https://api.openai.com"), "uris": []any{map[string]any{"uri": v.enc("https://ignored.example")}}},
				"fields": []any{
					map[string]any{"name": v.enc("org-id"), "value": v.enc("org_123"), "type": 0},
					map[string]any{"name": v.enc("a/b"), "value": v.enc("slashed")},
					map[string]any{"name": v.enc("empty-field"), "value": nil},
					map[string]any{"name": nil, "value": v.enc("nameless")},
				},
			},
			map[string]any{"id": "c-uris", "type": 1, "name": v.enc("uris-only"), "login": map[string]any{"uris": []any{map[string]any{"uri": v.enc("https://first.example")}, map[string]any{"uri": v.enc("https://second.example")}}}},
			map[string]any{"id": "c-dup-1", "type": 1, "name": v.enc("duplicate"), "login": map[string]any{"password": v.enc("pass1")}},
			map[string]any{"id": "c-dup-2", "type": 1, "name": v.enc("duplicate"), "login": map[string]any{"password": v.enc("pass2")}},
			map[string]any{"id": "c-note", "type": 2, "name": v.enc("note"), "notes": v.enc("just a note")},
			// Named after another item's id: a name match is tried before an id match.
			map[string]any{"id": "c-shadow", "type": 1, "name": v.enc("c-note"), "login": map[string]any{"password": v.enc("shadow")}},
			map[string]any{"id": "cipher-org", "type": 1, "organizationId": "org-abc", "name": org("org-secret"), "login": map[string]any{"password": org("org-password-value")}},
		},
	})}

	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		var rep reply
		call := r.Method + " " + r.URL.Path
		switch call {
		case "POST /identity/accounts/prelogin":
			rep = v.prelogin
			v.preloginBodies = append(v.preloginBodies, string(body)+" "+r.Header.Get("Content-Type"))
		case "POST /identity/connect/token":
			form, _ := url.ParseQuery(string(body))
			call += " " + form.Get("grant_type") + " " + form.Get("refresh_token")
			if rep = v.token; form.Get("grant_type") != "client_credentials" {
				rep = v.refresh
			}
			v.loginForms = append(v.loginForms, form)
		case "GET /api/sync":
			call += " " + r.Header.Get("Authorization")
			if rep = v.sync; len(v.syncSeq) > 0 {
				rep, v.syncSeq = v.syncSeq[0], v.syncSeq[1:]
			}
		default:
			rep = reply{404, "not found"}
		}
		v.calls = append(v.calls, call)
		gate := v.gate
		v.mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vault) config() Config {
	return Config{
		BaseURL: v.srv.URL + "/", Email: vaultEmail, MasterPassword: vaultPassword,
		ClientID: "client-id", ClientSecret: "client-secret", SyncInterval: time.Hour, DeviceID: "test-device",
	}
}

// open returns a resolver over the vault once its first sync has finished, well or badly.
func (v *vault) open(cfg Config) *Resolver {
	v.t.Helper()
	r := New(cfg)
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.RLock()
		done := r.syncDone
		r.mu.RUnlock()
		if done {
			return r
		}
		if time.Now().After(deadline) {
			v.t.Fatal("first sync never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

func (v *vault) wantCalls(want ...string) {
	v.t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if d := cmp.Diff(want, v.calls); d != "" {
		v.t.Errorf("requests to the vault (-want +got):\n%s", d)
	}
}

func resolve(r *Resolver, path string) (string, error) {
	got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindBitwarden, Path: path})
	return string(got), err
}

const (
	callPrelogin = "POST /identity/accounts/prelogin"
	callLogin    = "POST /identity/connect/token client_credentials "
	callSync     = "GET /api/sync Bearer access-token"
)

func TestResolver_Resolve(t *testing.T) {
	v := newVault(t)
	r := v.open(v.config())
	v.wantCalls(callPrelogin, callLogin, callSync)
	if d := cmp.Diff([]string{`{"email":"user@example.com"} application/json`}, v.preloginBodies); d != "" {
		t.Errorf("prelogin request (-want +got):\n%s", d)
	}
	wantLogin := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"client-id"}, "client_secret": {"client-secret"}, "scope": {"api"},
		"deviceType": {"14"}, "deviceIdentifier": {"test-device"}, "deviceName": {"wyolet-secret"},
	}
	if d := cmp.Diff([]url.Values{wantLogin}, v.loginForms); d != "" {
		t.Errorf("login form (-want +got):\n%s", d)
	}

	for _, tc := range []struct{ path, want, wantErr string }{
		{"openai-key", "sk-secret-value", ""},
		{" openai-key ", "sk-secret-value", ""},
		{"openai-key/password", "sk-secret-value", ""},
		{"openai-key/username", "api-user", ""},
		{"openai-key/uri", "https://api.openai.com", ""},
		{"openai-key/notes", "rotate monthly", ""},
		{"openai-key/org-id", "org_123", ""},
		{"openai-key/a/b", "slashed", ""},
		{"cipher-personal/username", "api-user", ""},
		{"uris-only/uri", "https://first.example", ""},
		{"note/notes", "just a note", ""},
		{"c-note/password", "shadow", ""},
		{"org-secret/password", "org-password-value", ""},
		{"cipher-org", "org-password-value", ""},
		{"", "", "bitwarden: empty path"},
		{"  ", "", "bitwarden: empty path"},
		{"/password", "", `bitwarden: empty item name in path "/password"`},
		{"openai-key/", "", `bitwarden: empty field in path "openai-key/"`},
		{"nope", "", `bitwarden: item "nope" not found`},
		{"OPENAI-KEY", "", `bitwarden: item "OPENAI-KEY" not found`},
		{"duplicate/password", "", `bitwarden: ambiguous item name "duplicate" (2 matches)`},
		{"openai-key/nope", "", `bitwarden: unknown field "nope" on item "openai-key"`},
		{"openai-key/nameless", "", `bitwarden: unknown field "nameless" on item "openai-key"`},
		{"openai-key/Password", "", `bitwarden: unknown field "Password" on item "openai-key"`},
		{"openai-key/empty-field", "", `bitwarden: field "empty-field" is empty or missing on item "openai-key"`},
		{"uris-only", "", `bitwarden: field "password" is empty or missing on item "uris-only"`},
		{"note/username", "", `bitwarden: field "username" is empty or missing on item "note"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got, err := resolve(r, tc.path); got != tc.want || !wantErr(err, tc.wantErr) {
				t.Errorf("Resolve = %q, %v; want %q, %q", got, err, tc.want, tc.wantErr)
			}
		})
	}

	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindEnv, Path: "openai-key"})
	if !wantErr(err, `bitwarden: wrong kind "env"`) {
		t.Errorf("Resolve of an env ref: %v", err)
	}
	v.wantCalls(callPrelogin, callLogin, callSync)
}

func TestResolver_TokenLifecycle(t *testing.T) {
	const (
		callRefresh    = "POST /identity/connect/token refresh_token refresh-token"
		callSyncFresh  = "GET /api/sync Bearer refreshed-token"
		shortLived     = `{"access_token":"access-token","expires_in":30,"refresh_token":"refresh-token","Key":`
		refreshRefused = "bitwarden: vault sync failed: bitwarden: sync auth failed: bitwarden: refresh failed (HTTP 400): revoked"
	)
	userKeyOf := func(v *vault) string {
		var tok struct{ Key string }
		if err := json.Unmarshal([]byte(v.token.body), &tok); err != nil {
			t.Fatal(err)
		}
		return jsonOf(t, tok.Key)
	}
	for _, tc := range []struct {
		name      string
		arrange   func(*vault)
		wantCalls []string
		wantErr   string
	}{
		{"a token about to expire is refreshed before the sync", func(v *vault) { v.token.body = shortLived + userKeyOf(v) + "}" },
			[]string{callPrelogin, callLogin, callRefresh, callSyncFresh}, ""},
		{"a refused refresh falls back to a new login", func(v *vault) { v.token.body, v.refresh = shortLived+userKeyOf(v)+"}", reply{400, "revoked"} },
			[]string{callPrelogin, callLogin, callRefresh, callPrelogin, callLogin, callSync}, ""},
		{"a 401 from sync refreshes the token and retries", func(v *vault) { v.syncSeq = []reply{{401, "expired"}} },
			[]string{callPrelogin, callLogin, callSync, callRefresh, callSyncFresh}, ""},
		{"a 401 from sync with the refresh refused", func(v *vault) { v.syncSeq, v.refresh = []reply{{401, "expired"}}, reply{400, "revoked"} },
			[]string{callPrelogin, callLogin, callSync, callRefresh}, refreshRefused},
		{"a second 401 is the answer", func(v *vault) { v.syncSeq = []reply{{401, "expired"}, {401, "still expired"}} },
			[]string{callPrelogin, callLogin, callSync, callRefresh, callSyncFresh}, "bitwarden: vault sync failed: bitwarden: sync failed (HTTP 401): still expired"},
		{"no key in the token: the profile key is read from a sync", func(v *vault) {
			v.syncSeq = []reply{{200, `{"profile":{"key":` + userKeyOf(v) + `}}`}}
			v.token.body = `{"access_token":"access-token","expires_in":3600,"refresh_token":"refresh-token"}`
		}, []string{callPrelogin, callLogin, callSync, callSync}, ""},
		{"no key in the token or the profile", func(v *vault) {
			v.syncSeq = []reply{{200, `{"profile":{}}`}}
			v.token.body = `{"access_token":"access-token","expires_in":3600}`
		}, []string{callPrelogin, callLogin, callSync}, "bitwarden: vault sync failed: bitwarden: fetch profile key: bitwarden: profile key is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVault(t)
			tc.arrange(v)
			got, err := resolve(v.open(v.config()), "openai-key")
			if want := map[bool]string{true: "sk-secret-value"}[tc.wantErr == ""]; got != want || !wantErr(err, tc.wantErr) {
				t.Errorf("Resolve = %q, %v; want %q, %q", got, err, want, tc.wantErr)
			}
			v.wantCalls(tc.wantCalls...)
			if len(v.loginForms) > 1 && v.loginForms[1].Get("grant_type") == "refresh_token" {
				want := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"refresh-token"}, "client_id": {"web"}}
				if d := cmp.Diff(want, v.loginForms[1]); d != "" {
					t.Errorf("refresh form (-want +got):\n%s", d)
				}
			}
		})
	}
}

func TestResolver_SyncFailures(t *testing.T) {
	const prefix = "bitwarden: vault sync failed: "
	for _, tc := range []struct {
		name    string
		arrange func(*vault, *Config)
		wantErr string
	}{
		{"prelogin refused", func(v *vault, _ *Config) { v.prelogin = reply{500, "boom"} }, "bitwarden: prelogin: prelogin failed (HTTP 500): boom"},
		{"prelogin is not json", func(v *vault, _ *Config) { v.prelogin = reply{200, "<html>"} }, "bitwarden: prelogin: invalid character '<' looking for beginning of value"},
		{"unknown kdf", func(v *vault, _ *Config) { v.prelogin = reply{200, `{"kdf":9,"kdfIterations":1000}`} }, "bitwarden: derive master key: bitwarden: unsupported KDF type: 9"},
		{"no client id", func(_ *vault, c *Config) { c.ClientID = "" }, "bitwarden: client_id and client_secret are required"},
		{"no client secret", func(_ *vault, c *Config) { c.ClientSecret = "" }, "bitwarden: client_id and client_secret are required"},
		{"login refused", func(v *vault, _ *Config) { v.token = reply{400, `{"error":"invalid_client"}`} }, `bitwarden: login: login failed (HTTP 400): {"error":"invalid_client"}`},
		{"login is not json", func(v *vault, _ *Config) { v.token = reply{200, "<html>"} }, "bitwarden: login: invalid character '<' looking for beginning of value"},
		{"wrong master password", func(_ *vault, c *Config) { c.MasterPassword = "Master-password" }, "bitwarden: decrypt symmetric key: bitwarden: decrypt symmetric key: bitwarden: MAC key required for type 2 decryption"},
		{"another account's email", func(_ *vault, c *Config) { c.Email = "other@example.com" }, "bitwarden: decrypt symmetric key: bitwarden: decrypt symmetric key: bitwarden: MAC key required for type 2 decryption"},
		{"sync refused", func(v *vault, _ *Config) { v.sync = reply{500, "down"} }, "bitwarden: sync failed (HTTP 500): down"},
		{"sync is not json", func(v *vault, _ *Config) { v.sync = reply{200, "<html>"} }, "bitwarden: decode sync response: invalid character '<' looking for beginning of value"},
		{"private key does not open", func(v *vault, _ *Config) {
			v.sync.body = `{"profile":{"privateKey":"junk","organizations":[{"id":"org-abc","key":"4.AAAA"}]}}`
		}, "bitwarden: decrypt RSA private key: bitwarden: parse private key cipher string: bitwarden: invalid cipher string: missing type separator"},
		{"org key does not open", func(v *vault, _ *Config) {
			v.sync.body = `{"profile":{"privateKey":` + jsonOf(v.t, encryptType2(v.t, v.userKey, mustPKCS8(v.t))) + `,"organizations":[{"id":"org-abc","key":""}]}}`
		}, "bitwarden: decrypt org key for org-abc: bitwarden: encrypted org key is empty"},
		{"cipher of an organization without a key", func(v *vault, _ *Config) {
			v.sync.body = `{"ciphers":[{"id":"cipher-org","organizationId":"org-abc","name":` + jsonOf(v.t, v.enc("x")) + `}]}`
		}, "bitwarden: no org key for cipher cipher-org (org org-abc)"},
		{"cipher that does not open", func(v *vault, _ *Config) {
			v.sync.body = `{"ciphers":[{"id":"c1","name":` + jsonOf(v.t, encryptType2String(v.t, symKey(50), "x")) + `}]}`
		}, "bitwarden: decrypt cipher c1: decrypt name: bitwarden: MAC verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVault(t)
			cfg := v.config()
			tc.arrange(v, &cfg)
			if got, err := resolve(v.open(cfg), "openai-key"); got != "" || !wantErr(err, prefix+tc.wantErr) {
				t.Errorf("Resolve = %q, %v; want %q", got, err, prefix+tc.wantErr)
			}
		})
	}
}

func mustPKCS8(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(testRSAKey())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// A later sync replaces the cache; a failed one is reported until the next good one.
func TestResolver_Resync(t *testing.T) {
	v := newVault(t)
	r := v.open(v.config())
	ctx := context.Background()

	v.mu.Lock()
	good := v.sync
	v.sync = reply{200, `{"ciphers":[{"id":"c-new","type":1,"name":` + jsonOf(t, v.enc("rotated")) + `,"login":{"password":` + jsonOf(t, v.enc("sk-new")) + `}}]}`}
	v.mu.Unlock()
	r.refresh(ctx)
	if got, err := resolve(r, "rotated"); got != "sk-new" || err != nil {
		t.Errorf("new item after a re-sync: %q, %v", got, err)
	}
	if _, err := resolve(r, "openai-key"); !wantErr(err, `bitwarden: item "openai-key" not found`) {
		t.Errorf("removed item after a re-sync: %v", err)
	}

	v.mu.Lock()
	v.sync = reply{500, "down"}
	v.mu.Unlock()
	r.refresh(ctx)
	if _, err := resolve(r, "rotated"); !wantErr(err, "bitwarden: vault sync failed: bitwarden: sync failed (HTTP 500): down") {
		t.Errorf("after a failed re-sync: %v", err)
	}

	v.mu.Lock()
	v.sync = good
	v.mu.Unlock()
	r.refresh(ctx)
	if got, err := resolve(r, "openai-key"); got != "sk-secret-value" || err != nil {
		t.Errorf("after the vault recovered: %q, %v", got, err)
	}
}

func TestResolver_WaitsForTheFirstSync(t *testing.T) {
	v := newVault(t)
	v.gate = make(chan struct{})
	r := New(v.config())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(cancelled, secret.Ref{Kind: secret.KindBitwarden, Path: "openai-key"}); err != context.Canceled {
		t.Errorf("Resolve with a cancelled context before the first sync: %v", err)
	}
	timed, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Resolve(timed, secret.Ref{Kind: secret.KindBitwarden, Path: "openai-key"}); err != context.DeadlineExceeded {
		t.Errorf("Resolve whose context ends during the first sync: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		got, err := resolve(r, "openai-key")
		if err != nil {
			got = err.Error()
		}
		done <- got
	}()
	close(v.gate)
	select {
	case got := <-done:
		if got != "sk-secret-value" {
			t.Errorf("Resolve started before the first sync = %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Resolve never returned after the first sync finished")
	}
}
