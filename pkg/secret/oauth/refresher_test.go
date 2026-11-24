package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

// memVault is an in-memory Vault: id → token blob.
type memVault struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (v *memVault) Resolve(_ context.Context, ref secret.Ref) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	b, ok := v.blobs[ref.ID]
	if !ok {
		return nil, fmt.Errorf("no blob %q", ref.ID)
	}
	return b, nil
}

func (v *memVault) Create(_ context.Context, id string, plaintext []byte) (secret.Ref, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.blobs[id] = plaintext
	return secret.Ref{Kind: secret.KindStored, ID: id}, nil
}

func storeToken(t *testing.T, v *memVault, id string, tok sdkoauth.Token) {
	t.Helper()
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	v.blobs[id] = b
}

// tokenEndpoint counts hits and answers with a fresh rotated token, or an
// RFC 6749 error when fail is set.
func tokenEndpoint(hits *int, fail string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		if fail != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, fail)
			return
		}
		fmt.Fprintf(w, `{"access_token":"at-new-%d","refresh_token":"rt-new-%d","token_type":"Bearer","expires_in":3600}`, *hits, *hits)
	}
}

func testRefresher(t *testing.T, tokenURL string, opts ...Option) (*Refresher, *memVault, *[]string) {
	t.Helper()
	vault := &memVault{blobs: map[string][]byte{}}
	res := NewResolver(vault, func(string) (sdkoauth.ProviderConfig, bool) {
		return sdkoauth.ProviderConfig{ClientID: "c", AuthURL: tokenURL, TokenURL: tokenURL}, true
	}, opts...)
	var events []string
	revoked := map[string]bool{}
	source := func(context.Context) ([]secret.Ref, error) {
		refs := make([]secret.Ref, 0, len(vault.blobs))
		for id := range vault.blobs {
			if revoked[id] { // composition-root contract: revoked stay out
				continue
			}
			refs = append(refs, secret.Ref{Kind: secret.KindOAuth, ID: id, Provider: "p"})
		}
		return refs, nil
	}
	hooks := Hooks{
		OnRenewed: func(_ context.Context, id string, expiresAt time.Time) error {
			if expiresAt.Before(time.Now()) {
				return fmt.Errorf("OnRenewed got past expiry %v", expiresAt)
			}
			events = append(events, "renewed:"+id)
			return nil
		},
		OnRevoked: func(_ context.Context, id string, cause error) error {
			revoked[id] = true
			events = append(events, "revoked:"+id)
			return nil
		},
	}
	f := NewRefresher(source, res, kv.NewMem(), hooks, slog.Default())
	return f, vault, &events
}

func TestRefresher_RenewsDueCredential(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(tokenEndpoint(&hits, ""))
	defer srv.Close()

	f, vault, notified := testRefresher(t, srv.URL)
	storeToken(t, vault, "k1", sdkoauth.Token{
		AccessToken: "at-old", RefreshToken: "rt-old",
		Expiry: time.Now().Add(5 * time.Minute), // inside the 10m lead
	})

	f.sweep(context.Background())

	if hits != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", hits)
	}
	if len(*notified) != 1 || (*notified)[0] != "renewed:k1" {
		t.Fatalf("events = %v, want [renewed:k1]", *notified)
	}
	var tok sdkoauth.Token
	if err := json.Unmarshal(vault.blobs["k1"], &tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-new-1" || tok.RefreshToken != "rt-new-1" {
		t.Fatalf("persisted blob not rotated: %+v", tok)
	}

	// Freshly renewed (expiry now beyond lead): second sweep is a no-op.
	f.sweep(context.Background())
	if hits != 1 {
		t.Fatalf("second sweep hit the endpoint (%d hits) — not due, must no-op", hits)
	}
}

// storeDueGrant stores a service-account credential whose token is inside the renewal lead.
func storeDueGrant(t *testing.T, vault *memVault, id, tokenURL string) *sdkoauth.JWTBearer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	grant := &sdkoauth.JWTBearer{
		Issuer:     "svc@example.test",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		TokenURL:   tokenURL,
		Scopes:     []string{"scope-a"},
	}
	b, err := json.Marshal(sdkoauth.Credential{
		Token:     sdkoauth.Token{AccessToken: "at-old", Expiry: time.Now().Add(5 * time.Minute)},
		JWTBearer: grant,
	})
	if err != nil {
		t.Fatal(err)
	}
	vault.blobs[id] = b
	return grant
}

func allowLocal(srv *httptest.Server) Option {
	return func(r *Resolver) {
		AllowLoopback(srv.Client().Transport.(*http.Transport).TLSClientConfig)(r)
		TokenHosts([]string{"127.0.0.1"})(r)
	}
}

// A renewal refuses a token URL at a non-public address or one that redirects, without following it, and reports the key revoked without naming the address.
func TestRefresher_RefusedTokenURLRevokes(t *testing.T) {
	followed := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/token", http.StatusTemporaryRedirect)
			return
		}
		followed++
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name, path string
		opts       []Option
	}{
		{"loopback address", "/token", []Option{TokenHosts([]string{"127.0.0.1"})}},
		{"redirect", "/redirect", []Option{allowLocal(srv)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, vault, _ := testRefresher(t, "http://unused", tc.opts...)
			var cause error
			f.hooks.OnRevoked = func(_ context.Context, _ string, err error) error { cause = err; return nil }
			storeDueGrant(t, vault, "k1", srv.URL+tc.path)
			f.sweep(context.Background())
			if cause == nil || !IsPermanent(cause) || strings.Contains(cause.Error(), "127.0.0.1") {
				t.Fatalf("revocation cause = %v, want a permanent refusal that names no address", cause)
			}
		})
	}
	if followed != 0 {
		t.Fatalf("the token endpoint was reached %d times, want 0", followed)
	}
}

// A renewal answered with a token without an expiry keeps the current token and retries on the next sweep.
func TestRefresher_TokenWithoutExpiryRetries(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"forever","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	f, vault, notified := testRefresher(t, "http://unused", allowLocal(srv))
	storeDueGrant(t, vault, "k1", srv.URL)
	before := string(vault.blobs["k1"])

	f.sweep(context.Background())
	f.sweep(context.Background())

	if hits != 2 || len(*notified) != 0 || string(vault.blobs["k1"]) != before {
		t.Fatalf("hits = %d, events = %v, blob changed %v; want 2 hits, no event, the old token kept", hits, *notified, string(vault.blobs["k1"]) != before)
	}
}

// A renewal answered with success but no access token is retried, never reported revoked.
func TestRefresher_SuccessWithoutAccessTokenRetries(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"x","token_type":"Bearer"}`))
	}))
	defer srv.Close()
	ctx := context.Background()

	f, vault, notified := testRefresher(t, "http://unused", allowLocal(srv))
	storeDueGrant(t, vault, "k1", srv.URL)

	f.sweep(ctx)
	f.sweep(ctx)

	if hits != 2 || len(*notified) != 0 {
		t.Fatalf("hits = %d, events = %v; want 2 hits and no event", hits, *notified)
	}
}

func TestRefresher_RenewsDueJWTBearerCredential(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(tokenEndpoint(&hits, ""))
	defer srv.Close()
	ctx := context.Background()

	// The provider config testRefresher wires points at an http URL; a grant never consults it.
	f, vault, notified := testRefresher(t, "http://unused", allowLocal(srv))
	grant := storeDueGrant(t, vault, "k1", srv.URL)

	f.sweep(ctx)

	if hits != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", hits)
	}
	if len(*notified) != 1 || (*notified)[0] != "renewed:k1" {
		t.Fatalf("events = %v, want [renewed:k1]", *notified)
	}
	var cred sdkoauth.Credential
	if err := json.Unmarshal(vault.blobs["k1"], &cred); err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "at-new-1" || cred.JWTBearer == nil || cred.JWTBearer.PrivateKey != grant.PrivateKey {
		t.Fatalf("persisted credential: access %q, grant kept %v", cred.AccessToken, cred.JWTBearer != nil)
	}

	f.sweep(ctx)
	if hits != 1 {
		t.Fatalf("second sweep hit the endpoint (%d hits) — not due, must no-op", hits)
	}
}

func TestRefresher_NotDueIsNoop(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(tokenEndpoint(&hits, ""))
	defer srv.Close()

	f, vault, notified := testRefresher(t, srv.URL)
	storeToken(t, vault, "k1", sdkoauth.Token{
		AccessToken: "at", RefreshToken: "rt",
		Expiry: time.Now().Add(2 * time.Hour),
	})

	f.sweep(context.Background())
	if hits != 0 || len(*notified) != 0 {
		t.Fatalf("hits=%d notified=%v, want no activity", hits, *notified)
	}
}

func TestRefresher_PermanentFailureParks(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(tokenEndpoint(&hits, "invalid_grant"))
	defer srv.Close()

	f, vault, notified := testRefresher(t, srv.URL)
	storeToken(t, vault, "k1", sdkoauth.Token{
		AccessToken: "at", RefreshToken: "rt",
		Expiry: time.Now().Add(time.Minute),
	})

	f.sweep(context.Background())
	f.sweep(context.Background())
	f.sweep(context.Background())

	if hits != 1 {
		t.Fatalf("token endpoint hits = %d, want 1 (revoked keys leave the source)", hits)
	}
	if len(*notified) != 1 || (*notified)[0] != "revoked:k1" {
		t.Fatalf("events = %v, want [revoked:k1]", *notified)
	}
}

func TestRefresher_TransientFailureRetries(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	f, vault, _ := testRefresher(t, srv.URL)
	storeToken(t, vault, "k1", sdkoauth.Token{
		AccessToken: "at", RefreshToken: "rt",
		Expiry: time.Now().Add(time.Minute),
	})

	f.sweep(context.Background())
	f.sweep(context.Background())

	if hits != 2 {
		t.Fatalf("token endpoint hits = %d, want 2 (transient errors retry every sweep)", hits)
	}
	_ = f // transient failures never fire OnRevoked — the source keeps listing the key
}
