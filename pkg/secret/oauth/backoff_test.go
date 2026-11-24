package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

// A read whose exchange failed answers the same error without calling the token endpoint until the backoff passes; the backoff doubles up to its cap, and a success or a new stored value starts over.
func TestResolve_FailedExchangeBacksOff(t *testing.T) {
	var hits, up atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if up.Load() == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"minted","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	f, vault, _ := testRefresher(t, "http://unused", allowLocal(srv))
	r := f.resolver
	clock := time.Now()
	r.now = func() time.Time { return clock }
	grant := storeDueGrant(t, vault, "k1", srv.URL)
	storeExpired := func() {
		b, err := json.Marshal(sdkoauth.Credential{JWTBearer: grant})
		if err != nil {
			t.Fatal(err)
		}
		vault.blobs["k1"] = b
	}
	storeExpired()
	ref := secret.Ref{Kind: secret.KindOAuth, ID: "k1", Provider: "service-account"}
	read := func(wantHits int32) {
		t.Helper()
		if _, err := r.Resolve(context.Background(), ref); err == nil {
			t.Fatal("Resolve succeeded against a failing endpoint")
		}
		if got := hits.Load(); got != wantHits {
			t.Fatalf("token endpoint hits = %d, want %d", got, wantHits)
		}
	}

	read(1)
	read(1) // inside the first 30s
	clock = clock.Add(31 * time.Second)
	read(2)
	clock = clock.Add(31 * time.Second)
	read(2) // the backoff is 60s now
	clock = clock.Add(30 * time.Second)
	read(3)
	for range 6 {
		clock = clock.Add(maxBackoff + time.Second)
		read(hits.Load() + 1)
	}
	if w := r.failures["k1"].wait; w != maxBackoff {
		t.Fatalf("backoff = %v, want the %v cap", w, maxBackoff)
	}

	// A new stored value (a rotate) is tried at once.
	grant = storeDueGrant(t, vault, "k1", srv.URL)
	storeExpired()
	read(hits.Load() + 1)

	// A success forgets the failures.
	clock = clock.Add(time.Minute)
	up.Store(1)
	if got, err := r.Resolve(context.Background(), ref); err != nil || string(got) != "minted" {
		t.Fatalf("Resolve after recovery = %q, %v", got, err)
	}
	if _, ok := r.failures["k1"]; ok {
		t.Fatal("a success left the failure remembered")
	}
}
