package control

import (
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds the control-plane chi router. Today it mounts only the
// login surface; CRUD migrates here in a follow-up.
func NewRouter(deps LoginDeps) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Post("/control/login", LoginHandler(deps))
	r.Post("/control/logout", LogoutHandler())
	r.Get("/control/whoami", WhoamiHandler(deps.SessionToken))

	return r
}
