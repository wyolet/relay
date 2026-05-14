package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORS_AllowedOrigin_Preflight(t *testing.T) {
	h := CORS("https://relay.wyolet.dev")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should short-circuit before next handler")
	}))
	req := httptest.NewRequest(http.MethodOptions, "/anything", nil)
	req.Header.Set("Origin", "https://relay.wyolet.dev")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://relay.wyolet.dev" {
		t.Fatalf("allow-origin: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow-credentials: %q", got)
	}
}

func TestCORS_DisallowedOrigin_PreflightForbidden(t *testing.T) {
	h := CORS("https://relay.wyolet.dev")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("disallowed preflight should not reach next handler")
	}))
	req := httptest.NewRequest(http.MethodOptions, "/anything", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed preflight must not echo allow-origin")
	}
}

func TestCORS_NoOrigin_PassesThrough(t *testing.T) {
	called := false
	h := CORS("https://relay.wyolet.dev")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("non-CORS request should pass through")
	}
}
