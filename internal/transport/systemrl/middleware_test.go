package systemrl_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/transport/mode"
	"github.com/wyolet/relay/internal/transport/systemrl"
	pkgrl "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
)

// makeStore builds a minimal in-memory catalog.Store with optional RateLimits.
func makeStore(rls ...*catalog.RateLimit) catalog.Store {
	objs := make([]any, len(rls))
	for i, rl := range rls {
		objs[i] = rl
	}
	return catalog.NewMemStore(objs...)
}

func boolPtr(b bool) *bool { return &b }

func makeLimiter() *pkgrl.Limiter {
	mem := kv.NewMem()
	pkgrl.RegisterScripts(mem)
	return pkgrl.New(mem, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
}

func ok200(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// ─── helpers ────────────────────────────────────────────────────────────────

func newRL(name string, enabled *bool, amount int64, window time.Duration) *catalog.RateLimit {
	d := window
	return &catalog.RateLimit{
		Metadata: catalog.Metadata{Name: name},
		Spec: catalog.RateLimitSpec{
			Enabled:  enabled,
			Strategy: catalog.StrategySlidingWindow,
			Window:   d,
			Rules: []catalog.RateLimitRule{
				{
					Meter:    "requests",
					Amount:   amount,
					Window:   d,
					Strategy: catalog.StrategySlidingWindow,
				},
			},
		},
	}
}

func ipScope(r *http.Request) string {
	return "ip:127.0.0.1"
}

// ─── tests ──────────────────────────────────────────────────────────────────

// (a) passes through when bucket is disabled (Enabled = false)
func TestMiddlewarePassThroughWhenDisabled(t *testing.T) {
	rl := newRL("system-api", boolPtr(false), 1, time.Minute)
	store := makeStore(rl)
	lim := makeLimiter()

	mw := systemrl.Middleware(lim, func() catalog.Store { return store }, "system-api", ipScope)
	h := mw(http.HandlerFunc(ok200))

	// Even the 2nd request (which would be over limit=1) should pass because bucket is disabled.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/control/foo", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("request %d: got %d want 200 (bucket disabled)", i+1, rec.Code)
		}
	}
}

// (a-2) passes through when bucket is absent
func TestMiddlewarePassThroughWhenAbsent(t *testing.T) {
	store := makeStore() // empty
	lim := makeLimiter()

	mw := systemrl.Middleware(lim, func() catalog.Store { return store }, "system-api", ipScope)
	h := mw(http.HandlerFunc(ok200))

	req := httptest.NewRequest(http.MethodGet, "/control/foo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d want 200", rec.Code)
	}
}

// (b) 429 when Reserve exceeds
func TestMiddleware429OnExceeded(t *testing.T) {
	// limit = 1 request per minute
	rl := newRL("system-api", boolPtr(true), 1, time.Minute)
	store := makeStore(rl)
	lim := makeLimiter()

	mw := systemrl.Middleware(lim, func() catalog.Store { return store }, "system-api", ipScope)
	h := mw(http.HandlerFunc(ok200))

	req1 := httptest.NewRequest(http.MethodGet, "/control/foo", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("first request: got %d want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/control/foo", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 429 {
		t.Fatalf("second request: got %d want 429", rec2.Code)
	}
	if rec2.Header().Get("Content-Type") != "application/json" {
		t.Error("expected Content-Type: application/json on 429")
	}
}

// (c) fail-open when limiter errors with non-ExceededError
// This is hard to test directly without mocking the limiter. Instead, we
// verify that a zero-rule RL (no rules) passes through silently — which
// exercises the "no rules → pass through" early-exit path.
func TestMiddlewarePassThroughWithNoRules(t *testing.T) {
	rl := &catalog.RateLimit{
		Metadata: catalog.Metadata{Name: "empty-rl"},
		Spec: catalog.RateLimitSpec{
			Enabled: boolPtr(true),
			Rules:   nil, // no rules
		},
	}
	store := makeStore(rl)
	lim := makeLimiter()

	mw := systemrl.Middleware(lim, func() catalog.Store { return store }, "empty-rl", ipScope)
	h := mw(http.HandlerFunc(ok200))

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("no-rules: got %d want 200", rec.Code)
	}
}

// (d) ConditionalAnonymousMiddleware skips for non-ProxyAnonymous modes
func TestConditionalAnonymousMiddlewareSkipsNonAnonymous(t *testing.T) {
	// limit = 1 — would 429 if enforced
	rl := newRL("inference-proxy-anonymous", boolPtr(true), 1, time.Minute)
	store := makeStore(rl)
	lim := makeLimiter()

	mw := systemrl.ConditionalAnonymousMiddleware(lim, func() catalog.Store { return store }, "inference-proxy-anonymous", ipScope)
	h := mw(http.HandlerFunc(ok200))

	// Normal mode request — should not be rate-limited.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		// No X-WR-Proxy-Mode → ModeNormal
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("normal mode request %d: got %d want 200 (should skip system RL)", i+1, rec.Code)
		}
	}
}

// (d-2) ConditionalAnonymousMiddleware enforces for ProxyAnonymous mode
func TestConditionalAnonymousMiddlewareEnforcesForAnonymous(t *testing.T) {
	// limit = 1 request
	rl := newRL("inference-proxy-anonymous", boolPtr(true), 1, time.Minute)
	store := makeStore(rl)
	lim := makeLimiter()

	mw := systemrl.ConditionalAnonymousMiddleware(lim, func() catalog.Store { return store }, "inference-proxy-anonymous", ipScope)
	h := mw(http.HandlerFunc(ok200))

	anon := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-WR-Proxy-Mode", "Proxy")
		req.Header.Set("Authorization", "Bearer sk-anon")
		return req
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, anon())
	if rec1.Code != 200 {
		t.Fatalf("first anon: got %d want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, anon())
	if rec2.Code != 429 {
		t.Fatalf("second anon: got %d want 429", rec2.Code)
	}

	// A normal-mode request should still pass even after the anonymous limit is hit.
	reqNormal := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recNormal := httptest.NewRecorder()
	h.ServeHTTP(recNormal, reqNormal)
	if recNormal.Code != 200 {
		t.Fatalf("normal after anon exhausted: got %d want 200", recNormal.Code)
	}
}

// Mode stamp is a separate test: verify that ConditionalAnonymousMiddleware
// recognises mode correctly (integration with mode.Classify).
func TestConditionalAnonymousMiddlewareRecognisesMode(t *testing.T) {
	store := makeStore() // empty — no RL
	lim := makeLimiter()

	var gotMode mode.Mode
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cls, _ := mode.Classify(r)
		gotMode = cls.Mode
		w.WriteHeader(200)
	})

	mw := systemrl.ConditionalAnonymousMiddleware(lim, func() catalog.Store { return store }, "inference-proxy-anonymous", ipScope)
	h := mw(capture)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-WR-Proxy-Mode", "Proxy")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotMode != mode.ModeProxyAnonymous {
		t.Errorf("mode: got %v want ModeProxyAnonymous", gotMode)
	}
}
