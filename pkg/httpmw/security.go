package httpmw

import "net/http"

// SecurityHeaders sets browser hardening headers for a listener that serves
// browser-facing JSON: no framing, no MIME sniffing, no cross-origin referrer,
// and a CSP that lets a response render nothing. Handlers that serve HTML set
// their own Content-Security-Policy over this default.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
