package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// TestReserveStoreUnavailableAnswers429 pins the response to a reserve the store could not answer: the shed's 429 and Retry-After, and no upstream call.
func TestReserveStoreUnavailableAnswers429(t *testing.T) {
	f := newHotPathFixture(t, true)
	f.store.reserveErr = fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)
	f.arm()

	w := f.do(t, "sk-wr-live")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Retry-After"); got != httpapi.RetryAfterShed {
		t.Fatalf("Retry-After = %q, want %q", got, httpapi.RetryAfterShed)
	}
	var env httpapi.APIError
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Err.Type != "rate_limit_error" || env.Err.Code != "overloaded" {
		t.Fatalf("error = %s/%s, want rate_limit_error/overloaded", env.Err.Type, env.Err.Code)
	}
	if n := f.upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream called %d times, want 0", n)
	}
}

// Proxy mode refuses an unchecked reserve the same way.
func TestProxyReserveStoreUnavailableAnswers429(t *testing.T) {
	w := httptest.NewRecorder()
	mapProxyErr(w, fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, context.DeadlineExceeded))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != httpapi.RetryAfterShed {
		t.Fatalf("status = %d, Retry-After = %q; want 429, %q", w.Code, w.Header().Get("Retry-After"), httpapi.RetryAfterShed)
	}
}
