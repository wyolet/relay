package httpmw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/pkg/httpmw"
)

func TestSecurityHeaders(t *testing.T) {
	json := httpmw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	rr := httptest.NewRecorder()
	json.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
	} {
		if got := rr.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	page := httpmw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
	}))
	rr = httptest.NewRecorder()
	page.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rr.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("handler CSP not kept: %q", got)
	}
}
