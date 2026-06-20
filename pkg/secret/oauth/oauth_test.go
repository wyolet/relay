package oauth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
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
