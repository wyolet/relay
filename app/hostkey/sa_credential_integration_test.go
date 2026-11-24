//go:build integration

// sa_credential_integration_test.go covers an oauth host key created from a service-account key file: the write exchanges the key for a token, the key resolves to that token, and a new value clears the renewal status. Run with: make test-integration.

package hostkey_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

func serviceAccountKeyFile(t *testing.T, tokenURL string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"type":           "service_account",
		"client_email":   "svc@example.test",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"private_key_id": "kid-1",
		"token_uri":      tokenURL,
		"scopes":         []string{"scope-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIntegration_ServiceAccountKeyFileCredential(t *testing.T) {
	var mints int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("assertion") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"minted-%d","token_type":"Bearer","expires_in":3600}`, atomic.AddInt32(&mints, 1))
	}))
	defer srv.Close()
	rejecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer rejecting.Close()

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, srv.Client())
	pool := storagetest.Pool(t)
	store, sec := hostKeyStore(t, pool, masterKey)
	sec.Registry.Register(secret.KindOAuth, pkgoauth.NewResolver(sec.Stored, func(string) (sdkoauth.ProviderConfig, bool) {
		return sdkoauth.ProviderConfig{}, false
	}))
	owner := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}

	k := storedKey(owner, hostkey.ValueKindOAuth, serviceAccountKeyFile(t, srv.URL))
	k.Spec.ValueFrom.Provider = "service-account"
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("create from a key file: %v", err)
	}
	got := get(t, store, k.Meta.ID)
	if got.Resolved != "minted-1" || got.Status.Unresolved != nil {
		t.Fatalf("resolved %q, unresolved %+v; want the minted token", got.Resolved, got.Status.Unresolved)
	}
	if out, _ := json.Marshal(got); strings.Contains(string(out), "PRIVATE KEY") || strings.Contains(string(out), "minted") {
		t.Fatalf("the read returns key material or the token: %s", out)
	}
	raw, err := sec.Stored.Resolve(stored.WithScope(ctx, hostkey.SecretScope(owner)), secret.Ref{Kind: secret.KindStored, ID: k.Meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	var cred sdkoauth.Credential
	if err := json.Unmarshal(raw, &cred); err != nil || cred.JWTBearer == nil || cred.AccessToken != "minted-1" {
		t.Fatalf("stored credential: token %q, grant kept %v, %v", cred.AccessToken, cred.JWTBearer != nil, err)
	}

	if err := store.SetCredentialStatus(ctx, k.Meta.ID, hostkey.CredentialStatus{State: hostkey.CredentialRevoked, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if c := get(t, store, k.Meta.ID).Status.Credential; c == nil || c.State != hostkey.CredentialRevoked {
		t.Fatalf("status before the rotate: %+v", c)
	}
	rotated := get(t, store, k.Meta.ID)
	rotated.Spec.Value = serviceAccountKeyFile(t, srv.URL)
	if err := store.Upsert(ctx, rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after := get(t, store, k.Meta.ID)
	if after.Status.Credential != nil || after.Resolved != "minted-2" {
		t.Fatalf("after the rotate: status %+v, resolved %q", after.Status.Credential, after.Resolved)
	}

	refused := storedKey(owner, hostkey.ValueKindOAuth, serviceAccountKeyFile(t, rejecting.URL))
	rejectCtx := context.WithValue(context.Background(), oauth2.HTTPClient, rejecting.Client())
	if err := store.Upsert(rejectCtx, refused); err == nil || !pkgoauth.IsPermanent(err) {
		t.Fatalf("a key the endpoint rejects: err = %v, want a permanent error", err)
	}
	if k, err := store.Get(ctx, refused.Meta.ID); err != nil || k != nil {
		t.Fatalf("a refused key left a row: %+v, %v", k, err)
	}
}
