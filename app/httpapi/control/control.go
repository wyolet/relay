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

	"github.com/wyolet/relay/app/httpapi"
)

// Deps is the typed dependency bundle for the admin plane. Grows as
// endpoints are ported. The catalog Stores and Catalog snapshot land here
// in Stage 3 (CRUD rewire).
type Deps struct{}

// Mount installs the control-plane huma API on r and registers all
// operations. Returns the huma.API so the caller can attach test-only ops.
func Mount(r chi.Router, d Deps) huma.API {
	httpapi.InstallErrorRewriter()

	cfg := huma.DefaultConfig("Wyolet Relay — Control", httpapi.Version)
	cfg.Info.Description = "Admin plane. Authentication, catalog CRUD, and " +
		"operational endpoints. Firewalled separately from the data plane."

	api := humachi.New(r, cfg)

	registerVersion(api)
	return api
}
