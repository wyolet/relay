// Package inference is the data-plane HTTP API: /v1/* and /healthz.
//
// Mount(r, deps) wires huma+chi onto an existing chi router and returns the
// huma.API for callers that want to register additional operations (tests,
// experimental endpoints). The package owns its huma.Config; main.go just
// constructs Deps and calls Mount.
package inference

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi"
)

// Deps is the typed dependency bundle for the data plane. Grows as endpoints
// are ported in subsequent stages (pipeline runner, catalog, key-pool, etc.).
// Keep it minimal; if a field is only used by one handler, plumb it through
// that handler's signature instead of widening Deps.
type Deps struct {
	Pinger Pinger
}

// Pinger reports backend health for /healthz. Storage satisfies this via its
// own Ping method; tests pass a stub.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Mount installs the data-plane huma API on r and registers all operations.
// Returns the huma.API so the caller can attach test-only operations.
func Mount(r chi.Router, d Deps) huma.API {
	httpapi.InstallErrorRewriter()

	cfg := huma.DefaultConfig("Wyolet Relay — Inference", httpapi.Version)
	cfg.Info.Description = "Data plane. /v1/* endpoints accept OpenAI- and " +
		"Anthropic-shape requests; bytes are forwarded to the upstream " +
		"provider with usage extracted from the response."

	api := humachi.New(r, cfg)

	registerHealth(api, d)
	return api
}
