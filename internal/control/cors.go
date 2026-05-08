package control

import "net/http"

// CORS returns a middleware that handles preflight (OPTIONS) and adds the
// CORS headers needed for a credentialed browser fetch from one of the
// allowed origins. Required because the control API is consumed by a
// frontend on a separate hostname (e.g. https://relay.wyolet.dev).
//
// allowedOrigins is matched exactly against the Origin header — wildcard
// origins are not allowed when Access-Control-Allow-Credentials is true,
// per the CORS spec.
func CORS(allowedOrigins ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if _, ok := allowed[origin]; ok {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Vary", "Origin")
				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "content-type, x-relay-admin-token, authorization")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			} else if r.Method == http.MethodOptions {
				// Disallowed origin doing a preflight — reject without echoing
				// any allow headers; browser will block the real request.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
