// Package inference is the data-plane HTTP API: /v1/* and /healthz.
//
// Mount(r, deps) wires huma+chi onto an existing chi router and returns
// the huma.API. The package owns its huma.Config; main.go constructs
// Deps and calls Mount.
package inference

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
)

// Deps is the typed dependency bundle for the data plane.
type Deps struct {
	// Pinger reports backend health for /healthz. Storage satisfies
	// this; tests can pass a stub.
	Pinger Pinger

	// Catalog is the in-memory snapshot used for relay-key auth lookup
	// and the /v1/models listing.
	Catalog *appcatalog.Catalog

	// Resolver translates inbound model+policy refs into a pipeline-
	// ready Plan against the snapshot.
	Resolver *routing.Resolver

	// Pipeline orchestrates one inference request end-to-end.
	Pipeline *pipeline.Pipeline

	// Adapters keys the wire-protocol implementation by adapter.Kind.
	// Handlers look up the binding's Adapter Kind here at request time.
	Adapters map[adapter.Kind]pipeline.Adapter
}

// Pinger reports backend health for /healthz. Storage satisfies this
// via its own Ping method; tests can supply a stub.
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
	cfg.OpenAPI.Components.Schemas = httpapi.NewRegistry()

	api := humachi.New(r, cfg)

	// /healthz is public.
	registerHealth(api, d)

	// All /v1/* operations require a valid relay key.
	authMW := huma.Middlewares{httpapi.HumaAuth(RelayKeyAuthMiddleware(d.Catalog))}
	registerChat(api, d, authMW)
	registerMessages(api, d, authMW)
	registerModels(api, d, authMW)

	return api
}
