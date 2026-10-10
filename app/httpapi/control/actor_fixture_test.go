package control

import (
	"net/http"

	"github.com/wyolet/relay/app/actor"
)

// withTestActor stands in for the session and admin-token middleware: a
// request whose header names a key of actors runs as that actor, any other
// request runs with no actor at all.
func withTestActor(header string, actors map[string]*actor.Actor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := actors[req.Header.Get(header)]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	}
}
