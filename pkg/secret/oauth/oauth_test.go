package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

type memVault struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemVault() *memVault { return &memVault{data: map[string][]byte{}} }

func (m *memVault) put(id string, tok sdkoauth.Token) {
	b, _ := json.Marshal(tok)
	m.mu.Lock()
	m.data[id] = b
	m.mu.Unlock()
}

func (m *memVault) get(id string) sdkoauth.Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t sdkoauth.Token
	_ = json.Unmarshal(m.data[id], &t)
	return t
}

func (m *memVault) putCredential(id string, c sdkoauth.Credential) {
	b, _ := json.Marshal(c)
	m.mu.Lock()
	m.data[id] = b
	m.mu.Unlock()
}

func (m *memVault) raw(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.data[id])
}

func (m *memVault) Resolve(_ context.Context, ref secret.Ref) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[ref.ID]
	if !ok {
		return nil, fmt.Errorf("not found: %s", ref.ID)
	}
	return append([]byte(nil), b...), nil
}

func (m *memVault) Create(_ context.Context, id string, pt []byte) (secret.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = append([]byte(nil), pt...)
	return secret.Ref{Kind: secret.KindStored, ID: id}, nil
}

func cfgFunc(tokenURL string) pkgoauth.ProviderConfigFunc {
	return func(provider string) (sdkoauth.ProviderConfig, bool) {
		if provider != "vendor" {
			return sdkoauth.ProviderConfig{}, false
		}
		return sdkoauth.ProviderConfig{
			ClientID: "cid",
			AuthURL:  "https://x/authorize",
			TokenURL: tokenURL,
		}, true
	}
}

func TestResolve_ValidPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("token endpoint must not be hit for a valid token")
	}))
	defer srv.Close()

	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "good", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)})

	r := pkgoauth.NewResolver(v, cfgFunc(srv.URL))
	got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "good" {
		t.Errorf("access token: want good, got %q", got)
	}
}

func TestResolve_ExpiredRefreshesAndPersistsRotated(t *testing.T) {
	var gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		gotRefresh = req.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"r2","token_type":"bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)})

	r := pkgoauth.NewResolver(v, cfgFunc(srv.URL))
	got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("access token: want fresh, got %q", got)
	}
	if gotRefresh != "r1" {
		t.Errorf("refresh token sent: want r1, got %q", gotRefresh)
	}
	// rotated blob persisted back to the vault
	stored := v.get("k1")
	if stored.AccessToken != "fresh" || stored.RefreshToken != "r2" {
		t.Errorf("persisted blob: got access=%q refresh=%q (rotation not saved)", stored.AccessToken, stored.RefreshToken)
	}
	if raw := v.raw("k1"); strings.Contains(raw, "jwtBearer") {
		t.Errorf("a token blob gained a jwtBearer member: %s", raw)
	}
}

func TestResolve_SingleFlight(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	var hits int32
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		once.Do(func() { close(started) })
		<-proceed
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"r2","token_type":"bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)})
	r := pkgoauth.NewResolver(v, cfgFunc(srv.URL))

	const n = 8
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
			results[i], errs[i] = string(got), err
		}(i)
	}

	<-started
	time.Sleep(50 * time.Millisecond) // let the other callers queue into the flight
	close(proceed)
	wg.Wait()

	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (single-flight)", h)
	}
	for i := range n {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != "fresh" {
			t.Errorf("goroutine %d: got %q, want fresh", i, results[i])
		}
	}
}

func TestResolve_WrongKind(t *testing.T) {
	r := pkgoauth.NewResolver(newMemVault(), cfgFunc("http://x"))
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindStored, ID: "k1"})
	if err == nil {
		t.Fatal("expected error for non-oauth kind")
	}
}

func TestResolve_ZeroExpiryPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("token endpoint must not be hit for a zero-expiry token")
	}))
	defer srv.Close()
	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "long-lived", RefreshToken: "r1"}) // zero Expiry
	r := pkgoauth.NewResolver(v, cfgFunc(srv.URL))
	got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "long-lived" {
		t.Errorf("got %q, want long-lived", got)
	}
}

func TestResolve_ExpiredNoRefreshToken(t *testing.T) {
	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}) // no refresh token
	r := pkgoauth.NewResolver(v, cfgFunc("http://unused"))
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
	if err == nil {
		t.Fatal("expected error when expired with no refresh token")
	}
}

func TestResolve_ProviderConfigMissing(t *testing.T) {
	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)})
	r := pkgoauth.NewResolver(v, cfgFunc("http://unused"))
	// Provider "other" is unconfigured (cfgFunc only knows "vendor").
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "other"})
	if err == nil {
		t.Fatal("expected error when provider config is missing")
	}
}

func TestResolve_RefreshEndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	v := newMemVault()
	v.put("k1", sdkoauth.Token{AccessToken: "stale", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)})
	r := pkgoauth.NewResolver(v, cfgFunc(srv.URL))
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "vendor"})
	if err == nil {
		t.Fatal("expected error when refresh endpoint rejects the refresh token")
	}
	// The stale blob must be left intact (no corruption on failed refresh).
	if stored := v.get("k1"); stored.RefreshToken != "revoked" {
		t.Errorf("stored blob changed on failed refresh: %+v", stored)
	}
}

var testKeyPEM = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
})

func jwtGrant(tokenURL string) *sdkoauth.JWTBearer {
	return &sdkoauth.JWTBearer{Issuer: "svc@example.test", PrivateKey: testKeyPEM(), TokenURL: tokenURL, Scopes: []string{"scope-a"}}
}

// allowServer admits srv, a local TLS test server, to a resolver's exchanges.
func allowServer(srv *httptest.Server) pkgoauth.Option {
	return pkgoauth.AllowLoopback(srv.Client().Transport.(*http.Transport).TLSClientConfig)
}

// tokenServer is a TLS token endpoint answering status with body, counting hits; allow admits it to a resolver.
func tokenServer(t *testing.T, status int, body string) (srv *httptest.Server, allow pkgoauth.Option, hits *int32) {
	t.Helper()
	hits = new(int32)
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, allowServer(srv), hits
}

const mintedBody = `{"access_token":"minted","token_type":"Bearer","expires_in":3600}`

func TestResolve_JWTBearerRenewsWithoutProviderConfigAndKeepsGrant(t *testing.T) {
	srv, allow, hits := tokenServer(t, http.StatusOK, mintedBody)
	v := newMemVault()
	v.putCredential("k1", sdkoauth.Credential{
		Token:     sdkoauth.Token{AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)},
		JWTBearer: jwtGrant(srv.URL),
	})
	r := pkgoauth.NewResolver(v, cfgFunc("http://unused"), allow)
	// "service-account" has no provider config: the grant in the blob is all it needs.
	got, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "service-account"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "minted" || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("got %q after %d hits, want minted after 1", got, atomic.LoadInt32(hits))
	}
	var stored sdkoauth.Credential
	if err := json.Unmarshal([]byte(v.raw("k1")), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "minted" || stored.JWTBearer == nil || stored.JWTBearer.PrivateKey != testKeyPEM() {
		t.Fatalf("persisted credential lost its token or grant: access %q, grant kept %v", stored.AccessToken, stored.JWTBearer != nil)
	}
}

func TestResolve_JWTBearerSingleFlight(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	var hits int32
	var once sync.Once
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		once.Do(func() { close(started) })
		<-proceed
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mintedBody))
	}))
	defer srv.Close()
	ctx := context.Background()

	v := newMemVault()
	v.putCredential("k1", sdkoauth.Credential{JWTBearer: jwtGrant(srv.URL)})
	r := pkgoauth.NewResolver(v, cfgFunc("http://unused"), allowServer(srv))

	const n = 8
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			got, err := r.Resolve(ctx, secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "service-account"})
			results[i], errs[i] = string(got), err
		}(i)
	}
	<-started
	time.Sleep(50 * time.Millisecond) // let the other callers queue into the flight
	close(proceed)
	wg.Wait()

	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (single-flight)", h)
	}
	for i := range n {
		if errs[i] != nil || results[i] != "minted" {
			t.Errorf("goroutine %d: %q, %v", i, results[i], errs[i])
		}
	}
}

func keyFile(tokenURL string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "service_account", "client_email": "svc@example.test", "private_key": testKeyPEM(),
		"private_key_id": "kid-1", "token_uri": tokenURL, "scopes": []string{"scope-a"},
	})
	return b
}

func prepare(raw []byte, opts ...pkgoauth.Option) ([]byte, error) {
	return pkgoauth.NewResolver(nil, nil, opts...).PrepareValue(context.Background(), raw)
}

func TestPrepareValue_KeyFileStoresMintedToken(t *testing.T) {
	srv, allow, _ := tokenServer(t, http.StatusOK, mintedBody)
	out, err := prepare(keyFile(srv.URL), allow)
	if err != nil {
		t.Fatalf("PrepareValue: %v", err)
	}
	var c sdkoauth.Credential
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "minted" || c.JWTBearer == nil || c.JWTBearer.Issuer != "svc@example.test" || c.JWTBearer.PrivateKeyID != "kid-1" {
		t.Fatalf("prepared credential: access %q grant %+v", c.AccessToken, c.JWTBearer != nil)
	}
}

func TestPrepareValue_RejectedGrantFailsTheWrite(t *testing.T) {
	srv, allow, _ := tokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"bad signature","echo":"BODY-ECHO"}`)
	_, err := prepare(keyFile(srv.URL), allow)
	if err == nil || !pkgoauth.IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent error", err)
	}
	if want := "the token endpoint rejected the credential with HTTP 400: invalid_grant (bad signature)"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}

func TestPrepareValue_SuccessWithoutAccessTokenFailsTheWrite(t *testing.T) {
	srv, allow, _ := tokenServer(t, http.StatusOK, `{"id_token":"x","token_type":"Bearer"}`)
	_, err := prepare(keyFile(srv.URL), allow)
	if !errors.Is(err, sdkoauth.ErrNoAccessToken) {
		t.Fatalf("err = %v, want ErrNoAccessToken", err)
	}
	if pkgoauth.IsPermanent(err) {
		t.Fatalf("IsPermanent(%v) = true; a renewal must retry it", err)
	}
	if err.Error() != sdkoauth.ErrNoAccessToken.Error() {
		t.Fatalf("message = %q, want %q", err, sdkoauth.ErrNoAccessToken)
	}
}

func TestPrepareValue_TransientFailureStoresGrantWithoutToken(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unavailable", `{}`, http.StatusServiceUnavailable},
		{"success that is not a JSON object", `<html>maintenance</html>`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, allow, _ := tokenServer(t, tc.status, tc.body)
			out, err := prepare(keyFile(srv.URL), allow)
			if err != nil {
				t.Fatalf("PrepareValue: %v", err)
			}
			var c sdkoauth.Credential
			if err := json.Unmarshal(out, &c); err != nil {
				t.Fatal(err)
			}
			if c.AccessToken != "" || c.JWTBearer == nil {
				t.Fatalf("prepared credential: access %q grant kept %v", c.AccessToken, c.JWTBearer != nil)
			}
		})
	}
}

// The token URL is caller-supplied: a save refuses one at a non-public address (decided on the resolved IP, so a hostname counts too) or one that redirects, never reaching it, and the reason names no address.
func TestPrepareValue_RefusesNonPublicAndRedirectingTokenURLs(t *testing.T) {
	var reached int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/token", http.StatusTemporaryRedirect)
			return
		}
		atomic.AddInt32(&reached, 1)
		_, _ = w.Write([]byte(mintedBody))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	for _, tc := range []struct {
		name, url string
		opts      []pkgoauth.Option
	}{
		{"loopback IP", srv.URL + "/token", nil},
		{"hostname resolving to loopback", fmt.Sprintf("https://localhost:%d/token", port), nil},
		{"link-local metadata address", "https://169.254.169.254/token", nil},
		{"private address", "https://10.0.0.1/token", nil},
		{"redirect", srv.URL + "/redirect", []pkgoauth.Option{allowServer(srv)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepare(keyFile(tc.url), tc.opts...)
			if err == nil || !pkgoauth.IsPermanent(err) {
				t.Fatalf("err = %v, want a permanent refusal", err)
			}
			if msg := err.Error(); strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "::1") || strings.Contains(msg, "10.0.0.1") {
				t.Fatalf("message names an address: %q", msg)
			}
		})
	}
	if n := atomic.LoadInt32(&reached); n != 0 {
		t.Fatalf("the token endpoint was reached %d times, want 0", n)
	}
}

func TestPrepareValue_TokenBlobPassesThrough(t *testing.T) {
	in := []byte(`{"access_token":"at","refresh_token":"rt","expiry":"2099-01-01T00:00:00Z"}`)
	out, err := prepare(in)
	if err != nil || string(out) != string(in) {
		t.Fatalf("got %s, %v; want the input unchanged", out, err)
	}
}

func TestIsPermanent_StatusClasses(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusUnauthorized:        true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
	} {
		err := &oauth2.RetrieveError{Response: &http.Response{StatusCode: status}}
		if got := pkgoauth.IsPermanent(err); got != want {
			t.Errorf("IsPermanent(%d) = %v, want %v", status, got, want)
		}
	}
}
