// Package control is the admin-plane HTTP API: /auth/*, CRUD across the
// eight catalog kinds, /version, /master-key/*, /reload.
//
// Mount(r, deps) wires huma+chi onto an existing chi router and returns the
// huma.API. main.go constructs Deps and calls Mount.
package control

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/internal/identity"
)

// Deps is the typed dependency bundle for the admin plane.
type Deps struct {
	// Identity is the YAML-backed user store used by /auth/login.
	Identity *identity.Store

	// Sessions is the cookie-backed session manager. Login/Logout write to
	// it; the session middleware (installed by Mount) reads from it.
	Sessions *session.Manager

	// AdminToken is the cleartext break-glass bearer. Empty disables the
	// bypass. Validated by AdminTokenMiddleware; not used directly by
	// handlers.
	AdminToken string

	// Authz is the policy-decision interface. Handlers call
	// d.Authz.Authorize before mutations; today's impl is permissive for
	// any authenticated caller.
	Authz authz.Authorizer

	// Catalog is the in-memory snapshot used for slug→id resolution on
	// reads. Writes go through Stores.
	Catalog *appcatalog.Catalog

	// Stores is the bundle of eight typed stores used by CRUD writes.
	Stores *appcatalog.Stores

	// CookieSecure controls the Secure attribute on the session cookie.
	// Surfaced here so the OpenAPI doc can reflect deployment posture.
	CookieSecure bool
}

// Mount installs the control-plane huma API on r and registers all
// operations. Returns the huma.API so the caller can attach test-only ops.
//
// Middleware order on r (caller is responsible for wiring these onto the
// chi router before any operations are matched):
//
//  1. session.Manager.Middleware  — loads cookie session, sets Actor in ctx
//  2. AdminTokenMiddleware        — sets Actor in ctx when bearer matches
//
// Per-operation Middlewares attached by Mount enforce RequireActor on
// every protected route. Public ops (/auth/login, /version) omit it.
func Mount(r chi.Router, d Deps) huma.API {
	httpapi.InstallErrorRewriter()

	// Wire session + admin-token middlewares onto the chi router so all
	// downstream operations see Actor in context when present.
	if d.Sessions != nil {
		r.Use(d.Sessions.Middleware)
	}
	r.Use(AdminTokenMiddleware(d.AdminToken))

	cfg := huma.DefaultConfig("Wyolet Relay — Control", httpapi.Version)
	cfg.Info.Description = "Admin plane. Authentication, catalog CRUD, and " +
		"operational endpoints. Firewalled separately from the data plane."
	cfg.OpenAPI.Components.Schemas = httpapi.NewRegistry()
	api := humachi.New(r, cfg)

	// Protected-route middleware: every op that mutates state or returns
	// data goes through RequireActor (via humaAuth-style adapter). Public
	// endpoints omit it.
	protect := huma.Middlewares{httpapi.HumaAuth(RequireActor)}

	registerVersion(api)            // public
	registerAuth(api, d)            // /auth/login is public; whoami/logout don't need protect (whoami returns 401 itself)
	registerMisc(api, d, protect)   // /master-key/generate, /reload
	registerCRUD(api, d, protect)   // 8 kinds × CRUD

	return api
}
