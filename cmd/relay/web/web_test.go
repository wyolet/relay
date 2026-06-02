package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPresentAndServe(t *testing.T) {
	if !Present() {
		t.Skip("no real dist embedded (source build)")
	}
	h := Handler()
	// index fallback for a client route
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "<!doctype html") && !strings.Contains(strings.ToLower(rr.Body.String()), "<!doctype html") {
		t.Fatalf("client route should serve index.html, got %d body=%.80q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Cache-Control"); ct != "no-cache" {
		t.Fatalf("index should be no-cache, got %q", ct)
	}
}
