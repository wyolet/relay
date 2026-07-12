package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/secret"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
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

func testRefresher(t *testing.T, tokenURL string) (*Refresher, *memVault, *[]string) {
	t.Helper()
	vault := &memVault{blobs: map[string][]byte{}}
	res := NewResolver(vault, func(string) (sdkoauth.ProviderConfig, bool) {
		return sdkoauth.ProviderConfig{ClientID: "c", AuthURL: tokenURL, TokenURL: tokenURL}, true
	})
	var notified []string
	source := func(context.Context) ([]secret.Ref, error) {
		refs := make([]secret.Ref, 0, len(vault.blobs))
		for id := range vault.blobs {
			refs = append(refs, secret.Ref{Kind: secret.KindOAuth, ID: id, Provider: "p"})
		}
		return refs, nil
	}
	f := NewRefresher(source, res, kv.NewMem(), func(_ context.Context, id string) error {
		notified = append(notified, id)
		return nil
	}, slog.Default())
	return f, vault, &notified
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
	if len(*notified) != 1 || (*notified)[0] != "k1" {
		t.Fatalf("notify = %v, want [k1]", *notified)
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
		t.Fatalf("token endpoint hits = %d, want 1 (parked after invalid_grant)", hits)
	}
	if len(*notified) != 0 {
		t.Fatalf("notify = %v, want none", *notified)
	}
	if _, parked := f.dead["k1"]; !parked {
		t.Fatal("credential not parked in dead map")
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
	if _, parked := f.dead["k1"]; parked {
		t.Fatal("transient failure must not park the credential")
	}
}
