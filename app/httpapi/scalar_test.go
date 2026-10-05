package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The docs page shares the control origin with the session-authenticated
// API, so its only third-party script must be an exact version under SRI
// and the page must carry a CSP that allows nothing else.
func TestScalarDocsPinsScriptWithIntegrityAndCSP(t *testing.T) {
	h := ScalarHandler("Wyolet Relay — Control", "openapi.json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	tag := regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"[^>]*>`).FindStringSubmatch(rec.Body.String())
	if tag == nil {
		t.Fatal("no external script tag in body")
	}
	src, full := tag[1], tag[0]
	if !regexp.MustCompile(`@scalar/api-reference@\d+\.\d+\.\d+/`).MatchString(src) {
		t.Errorf("script %q is not pinned to an exact version", src)
	}
	if !strings.Contains(full, `integrity="sha384-`) || !strings.Contains(full, "crossorigin=") {
		t.Errorf("script tag lacks SRI: %s", full)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "connect-src 'self'", "script-src 'unsafe-eval' " + src + ";"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}
