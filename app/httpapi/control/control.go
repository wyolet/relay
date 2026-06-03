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

	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/usagelog"
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

	// UsageReader serves /usage/* read-side endpoints. nil disables
	// them — useful for deployments where usage events are consumed
	// from a separate store.
	UsageReader usagelog.Reader

	// PayloadReader serves /payloads/* read-side endpoints (the Logs view).
	// nil disables them — e.g. minimal builds or deployments where captured
	// bodies are consumed from a separate store.
	PayloadReader payloadlog.Reader

	// Selector is the keypool circuit-breaker owner, shared with the data
	// plane. The host-key health endpoint reads per-key breaker state through
	// it. nil disables that endpoint.
	Selector *keypool.Selector
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
	cfg.DocsPath = ""
	r.Get("/docs", httpapi.ScalarHandler("Wyolet Relay — Control", "/openapi.json"))
	api := humachi.New(r, cfg)

	// Protected-route middleware: every op that mutates state or returns
	// data goes through RequireActor (via humaAuth-style adapter). Public
	// endpoints omit it.
	protect := huma.Middlewares{httpapi.HumaAuth(RequireActor)}

	registerVersion(api)          // public
	registerAuth(api, d)          // /auth/login is public; whoami/logout don't need protect (whoami returns 401 itself)
	registerMisc(api, d, protect) // /master-key/generate, /reload
	registerCRUD(api, d, protect) // 8 kinds × CRUD
	registerHostKeyRotate(api, d, protect)
	registerHostKeyHealth(api, d, protect)
	registerReferences(api, d, protect)
	registerPolicyRelayKeys(api, d, protect)
	registerSettings(api, d, protect)
	registerResolve(api, d, protect)
	registerSubresources(api, d, protect) // /models/{ref}/hosts, /models/{ref}/pricing, /hosts/{ref}/models
	registerCatalogGraph(api, d, protect)
	registerDebug(api, d, protect)
	registerUsage(api, d, protect)
	registerLogs(api, d, protect)

	// OpenAPI shim: enrich generated schemas with metadata the domain types
	// deliberately don't carry (no huma tags in app/ratelimit). The spec is
	// marshalled lazily on first /openapi.json hit, so patching here lands.
	// `meter` enum comes from the domain const set; `window` is documented as
	// seconds (ratelimit.Window marshals as integer seconds). No-op if renamed.
	patchProp(api, "RateLimitRule", "meter", func(s *huma.Schema) { s.Enum = meterEnum() })
	patchProp(api, "RateLimitRule", "window", func(s *huma.Schema) {
		s.Description = "Measurement period, in whole seconds."
	})

	return api
}

// meterEnum projects the domain's closed meter set to []any for huma.Schema.Enum.
func meterEnum() []any {
	out := make([]any, len(ratelimit.AllMeters))
	for i, m := range ratelimit.AllMeters {
		out[i] = string(m)
	}
	return out
}

// patchProp mutates one property of a named generated schema. This is the
// openapi-shim seam: it keeps schema metadata (enums, units) out of the domain
// types and off the hot path. Safe no-op if the schema or property is absent.
func patchProp(api huma.API, schema, prop string, fn func(*huma.Schema)) {
	s := api.OpenAPI().Components.Schemas.SchemaFromRef("#/components/schemas/" + schema)
	if s == nil {
		return
	}
	if p, ok := s.Properties[prop]; ok && p != nil {
		fn(p)
	}
}
