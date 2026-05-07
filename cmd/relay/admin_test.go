package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/kv"
)

type mockReloader struct {
	called bool
	err    error
}

func (m *mockReloader) Reload(_ context.Context) error {
	m.called = true
	return m.err
}

func newTestLimiter() *ratelimit.Limiter {
	st := kv.NewMem()
	return ratelimit.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func newAdminRouter(tok string, store reloader, lim *ratelimit.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	if tok != "" && store != nil {
		r.Post("/admin/reload", adminReloadHandler(tok, store, lim))
	}
	return r
}

func TestAdminReload_TokenUnset_Returns404(t *testing.T) {
	// No token → endpoint not registered → 404.
	r := newAdminRouter("", nil, newTestLimiter())
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestAdminReload_MissingToken_Returns404(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("secret", store, newTestLimiter())
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	if store.called {
		t.Error("Reload should not have been called")
	}
}

func TestAdminReload_WrongToken_Returns404(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("correct", store, newTestLimiter())
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	if store.called {
		t.Error("Reload should not have been called")
	}
}

func TestAdminReload_CorrectToken_Returns200(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("secret", store, newTestLimiter())
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	if !store.called {
		t.Error("Reload should have been called")
	}
}

func TestAdminReload_ReloadError_Returns500(t *testing.T) {
	store := &mockReloader{err: errors.New("db gone")}
	r := newAdminRouter("secret", store, newTestLimiter())
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

func TestAdminReload_YAMLBackend_Returns404(t *testing.T) {
	// When pgStoreForAdmin is nil (YAML backend), endpoint is not registered.
	r := newAdminRouter("", nil, newTestLimiter()) // token set but no store simulated by not registering
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// TestAdminReload_RateLimit_10RPM verifies that the 11th request within the
// window receives HTTP 429 with Retry-After and OpenAI envelope.
func TestAdminReload_RateLimit_10RPM(t *testing.T) {
	store := &mockReloader{}
	lim := newTestLimiter()
	r := newAdminRouter("secret", store, lim)

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.RemoteAddr = "1.2.3.4:9999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// First 10 should succeed.
	for i := 0; i < 10; i++ {
		w := doReq()
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i+1, w.Code)
		}
	}
	// 11th should be rate-limited.
	w := doReq()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing on 429")
	}
	var env map[string]map[string]string
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if env["error"]["type"] != "rate_limit_exceeded" {
		t.Errorf("unexpected error type: %q", env["error"]["type"])
	}
	if env["error"]["code"] != "admin_rate_limit_exceeded" {
		t.Errorf("unexpected error code: %q", env["error"]["code"])
	}
}

// TestAdminReload_RateLimit_DifferentIPs verifies that different source IPs
// are tracked independently (X-Forwarded-For).
func TestAdminReload_RateLimit_DifferentIPs(t *testing.T) {
	store := &mockReloader{}
	lim := newTestLimiter()
	r := newAdminRouter("secret", store, lim)

	doReqIP := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("X-Forwarded-For", ip)
		req.RemoteAddr = "10.0.0.1:9999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Exhaust 10 RPM for IP A.
	for i := 0; i < 10; i++ {
		if w := doReqIP("192.168.1.1"); w.Code != http.StatusOK {
			t.Fatalf("ip A request %d: want 200, got %d", i+1, w.Code)
		}
	}
	// IP A is now rate-limited.
	if w := doReqIP("192.168.1.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("ip A: want 429, got %d", w.Code)
	}
	// IP B should still be allowed.
	if w := doReqIP("192.168.1.2"); w.Code != http.StatusOK {
		t.Fatalf("ip B: want 200, got %d (different IPs should be tracked separately)", w.Code)
	}
}

// TestAdminReload_RateLimit_EnvOverride verifies RELAY_ADMIN_RELOAD_RPM=3 produces
// 429 on the 4th request.
func TestAdminReload_RateLimit_EnvOverride(t *testing.T) {
	t.Setenv("RELAY_ADMIN_RELOAD_RPM", "3")

	store := &mockReloader{}
	lim := newTestLimiter()
	r := newAdminRouter("secret", store, lim)

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.RemoteAddr = "5.6.7.8:9999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	for i := 0; i < 3; i++ {
		if w := doReq(); w.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i+1, w.Code)
		}
	}
	if w := doReq(); w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request: want 429, got %d", w.Code)
	}
}
