//go:build integration

// store_unavailable_integration_test.go covers admission against a Redis whose connection pool is exhausted.
// Run with: make test-integration.

package inference

import (
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// An admission batch that never gets a pooled connection answers the shed's 429, recorded as kv_unavailable, rather than a 502.
func TestAdmitPoolTimeoutAnswers429(t *testing.T) {
	f := newHotPathFixtureOn(t, true, kvtest.ExhaustedPool(t, kvtest.Config(t)))
	f.arm()

	w := f.do(t, "sk-wr-live")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Retry-After"); got != httpapi.RetryAfterShed {
		t.Fatalf("Retry-After = %q, want %q", got, httpapi.RetryAfterShed)
	}
	if n := f.upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream called %d times, want 0", n)
	}
	select {
	case <-f.postFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("no post-flight event for the refused request")
	}
	if kind, _ := f.errorKind.Load().(string); kind != "kv_unavailable" {
		t.Fatalf("error kind = %q, want kv_unavailable", kind)
	}
}
