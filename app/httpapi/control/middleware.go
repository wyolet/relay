package control

import (
	"crypto/subtle"
	"net/http"

	"github.com/wyolet/relay/app/actor"
)

// AdminTokenMiddleware accepts the request iff it carries a valid bearer
// admin token in the Authorization header. On success it injects an Actor
// with AdminToken=true into context so downstream handlers + authz see a
// valid caller. On absent/mismatched token it does nothing — the next
// middleware in line (session) may still authenticate.
//
// adminToken is the cleartext token from RELAY_ADMIN_TOKEN. Empty disables
// the bypass entirely.
func AdminTokenMiddleware(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" {
				next.ServeHTTP(w, r)
				return
			}
			got := bearer(r.Header.Get("Authorization"))
			if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(adminToken)) == 1 {
				a := &actor.Actor{AdminToken: true, Username: "admin-token"}
				next.ServeHTTP(w, r.WithContext(actor.WithActor(r.Context(), a)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireActor blocks any request that has no Actor in context. Stacked
// after AdminTokenMiddleware and the session middleware so either auth
// path can satisfy it.
func RequireActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !actor.From(r.Context()).IsAuthenticated() {
			http.Error(w, `{"error":{"type":"authentication_error","message":"unauthenticated"}}`, http.StatusUnauthorized)
			w.Header().Set("Content-Type", "application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the value after "Bearer " in an Authorization header.
// Empty string when the header is missing or malformed.
func bearer(h string) string {
	const prefix = "Bearer "
	if len(h) < len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}
