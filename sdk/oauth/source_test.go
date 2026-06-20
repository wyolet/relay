package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/wyolet/relay/sdk/oauth"
)

func refreshServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"r2","token_type":"bearer","expires_in":3600}`))
	}))
}

func sourceCfg(tokenURL string) *oauth2.Config {
	return oauth.ProviderConfig{ClientID: "cid", AuthURL: "https://x/a", TokenURL: tokenURL}.OAuth2()
}

func TestTokenSource_ValidNoRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("must not refresh a valid token")
	}))
	defer srv.Close()

	var saved int32
	ts := oauth.New(sourceCfg(srv.URL)).TokenSource(
		&oauth.Token{AccessToken: "good", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)},
		oauth.PersistFunc(func(context.Context, *oauth.Token) error { atomic.AddInt32(&saved, 1); return nil }),
	)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "good" {
		t.Errorf("got %q, want good", tok.AccessToken)
	}
	if saved != 0 {
		t.Errorf("persister called %d times for a valid token, want 0", saved)
	}
}

func TestTokenSource_RefreshesAndPersistsRotation(t *testing.T) {
	srv := refreshServer(t, nil)
	defer srv.Close()

	var savedTok *oauth.Token
	ts := oauth.New(sourceCfg(srv.URL)).TokenSource(
		&oauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)},
		oauth.PersistFunc(func(_ context.Context, tk *oauth.Token) error { savedTok = tk; return nil }),
	)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "fresh" {
		t.Errorf("got %q, want fresh", tok.AccessToken)
	}
	if savedTok == nil || savedTok.RefreshToken != "r2" {
		t.Errorf("rotated refresh token not persisted: %+v", savedTok)
	}
}

func TestTokenSource_NilPersister(t *testing.T) {
	srv := refreshServer(t, nil)
	defer srv.Close()

	ts := oauth.New(sourceCfg(srv.URL)).TokenSource(
		&oauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)},
		nil, // in-memory only
	)
	got, err := ts.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "fresh" {
		t.Errorf("got %q, want fresh", got)
	}
}

func TestTokenSource_PersisterErrorPropagates(t *testing.T) {
	srv := refreshServer(t, nil)
	defer srv.Close()

	wantErr := context.DeadlineExceeded
	ts := oauth.New(sourceCfg(srv.URL)).TokenSource(
		&oauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)},
		oauth.PersistFunc(func(context.Context, *oauth.Token) error { return wantErr }),
	)
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("expected persister error to propagate")
	}
}

func TestTokenSource_ConcurrentSingleRefresh(t *testing.T) {
	var hits int32
	srv := refreshServer(t, &hits)
	defer srv.Close()

	ts := oauth.New(sourceCfg(srv.URL)).TokenSource(
		&oauth.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)},
		nil,
	)
	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if _, err := ts.Token(context.Background()); err != nil {
				t.Errorf("Token: %v", err)
			}
		}()
	}
	wg.Wait()
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Errorf("refreshed %d times, want 1 (serialized)", h)
	}
}
