//go:build integration

package control

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

// An oauth value the operator got wrong answers 400 with the reason and never the value; an unreachable token endpoint still stores the key.
func TestHostKeyCreateRefusesABadOAuthValue(t *testing.T) {
	tokenEndpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/reject":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad signature"}`))
		case "/id-token-only":
			_, _ = w.Write([]byte(`{"id_token":"x","token_type":"Bearer"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer tokenEndpoint.Close()

	pool := storagetest.Pool(t)
	ctx := t.Context()
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool, MasterKey: bytes.Repeat([]byte{0x5a}, 32)})
	must(t, "bootstrap stores", err)
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "sa-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	must(t, "upsert host", stores.Host.Upsert(ctx, h))
	acct := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "sa-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: h.Meta.ID}}
	must(t, "upsert account", stores.Account.Upsert(ctx, acct))
	_, err = cat.Hydrate(ctx, stores, appcatalog.BootstrapOptions{Pool: pool})
	must(t, "hydrate", err)

	// The token endpoint is a local TLS test server, which the exchange client refuses unless told otherwise.
	oauthResolver := pkgoauth.NewResolver(stores.Stored, func(string) (sdkoauth.ProviderConfig, bool) { return sdkoauth.ProviderConfig{}, false },
		pkgoauth.AllowLoopback(tokenEndpoint.Client().Transport.(*http.Transport).TLSClientConfig))
	stores.Secrets.Register(secret.KindOAuth, oauthResolver)
	stores.HostKey.UseOAuth(oauthResolver)

	deps := mountDeps(t)
	deps.Stores, deps.Catalog = stores, cat
	r := chi.NewRouter()
	Mount(r, deps)

	k, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, "generate key", err)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	must(t, "marshal key", err)
	keyFile := func(tokenURI string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "service_account", "client_email": "svc@example.test", "private_key_id": "kid-1",
			"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
			"token_uri":   tokenURI, "scopes": []string{"scope-a"},
		})
		return string(b)
	}
	create := func(t *testing.T, name, value string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"name": name, "owner": map[string]string{"kind": "system"}},
			"spec": map[string]any{
				"hostId": h.Meta.ID, "accountId": acct.Meta.ID,
				"valueFrom": map[string]string{"kind": "oauth", "provider": "service-account"},
				"value":     value,
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/host-keys", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	errorBody := func(t *testing.T, w *httptest.ResponseRecorder) (typ, msg string) {
		t.Helper()
		var b struct {
			Error struct{ Type, Message string } `json:"error"`
		}
		must(t, "decode error", json.Unmarshal(w.Body.Bytes(), &b))
		return b.Error.Type, b.Error.Message
	}

	t.Run("malformed key file", func(t *testing.T) {
		w := create(t, "sa-malformed", `{"type":"service_account","private_key":"VALUE-MARKER`)
		wantStatus(t, w, http.StatusBadRequest)
		typ, msg := errorBody(t, w)
		if typ != "invalid_request_error" || !strings.Contains(msg, "not valid JSON") || strings.Contains(msg, "VALUE-MARKER") {
			t.Fatalf("error %s: %q", typ, msg)
		}
	})
	t.Run("token endpoint rejects the key", func(t *testing.T) {
		w := create(t, "sa-rejected", keyFile(tokenEndpoint.URL+"/reject"))
		wantStatus(t, w, http.StatusBadRequest)
		typ, msg := errorBody(t, w)
		if typ != "invalid_request_error" || !strings.Contains(msg, "invalid_grant") || !strings.Contains(msg, "400") || strings.Contains(msg, "PRIVATE KEY") {
			t.Fatalf("error %s: %q", typ, msg)
		}
	})
	t.Run("token endpoint returns no access token", func(t *testing.T) {
		w := create(t, "sa-no-token", keyFile(tokenEndpoint.URL+"/id-token-only"))
		wantStatus(t, w, http.StatusBadRequest)
		typ, msg := errorBody(t, w)
		if typ != "invalid_request_error" || !strings.Contains(msg, "no access token") || !strings.Contains(msg, "scopes") {
			t.Fatalf("error %s: %q", typ, msg)
		}
	})
	t.Run("token endpoint unreachable", func(t *testing.T) {
		w := create(t, "sa-later", keyFile(tokenEndpoint.URL+"/down"))
		wantStatus(t, w, http.StatusCreated)
		if strings.Contains(w.Body.String(), "PRIVATE KEY") {
			t.Fatalf("the create answer carries key material: %s", w.Body)
		}
		var created struct {
			Metadata meta.Metadata `json:"metadata"`
		}
		must(t, "decode host key", json.Unmarshal(w.Body.Bytes(), &created))

		// The key has no token yet; a metadata-only edit keeps its stored credential.
		body, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"name": "sa-later", "displayName": "Renamed", "owner": map[string]string{"kind": "system"}},
			"spec": map[string]any{
				"hostId": h.Meta.ID, "accountId": acct.Meta.ID,
				"valueFrom": map[string]string{"kind": "oauth", "provider": "service-account"},
			},
		})
		req := httptest.NewRequest(http.MethodPut, "/host-keys/by-id/"+created.Metadata.ID, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		req.Header.Set("Content-Type", "application/json")
		put := httptest.NewRecorder()
		r.ServeHTTP(put, req)
		wantStatus(t, put, http.StatusOK)
		edited, err := stores.HostKey.Get(ctx, created.Metadata.ID)
		must(t, "get host key", err)
		if edited.Meta.DisplayName != "Renamed" {
			t.Fatalf("displayName = %q, want the edit applied", edited.Meta.DisplayName)
		}
		raw, err := stores.Stored.Resolve(stored.WithScope(ctx, hostkey.SecretScope(edited.Meta.Owner)), secret.Ref{Kind: secret.KindStored, ID: created.Metadata.ID})
		if err != nil || !strings.Contains(string(raw), "jwtBearer") {
			t.Fatalf("stored credential after the edit: %v (grant kept %v)", err, strings.Contains(string(raw), "jwtBearer"))
		}
	})
}
