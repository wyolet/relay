// Package inference is the data-plane HTTP API: /v1/* and /healthz.
//
// Mount(r, deps) wires huma+chi onto an existing chi router and returns
// the huma.API. The package owns its huma.Config; main.go constructs
// Deps and calls Mount.
package inference

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/routing"
)

// RouteMounter is what an adapter package exposes to mount its own
// inbound HTTP surface. Each adapter (openai, anthropic, ...) provides
// a MountRoutes(api, deps, mw) function matching this signature.
type RouteMounter = func(api huma.API, d Deps, mw huma.Middlewares)

// CrossShapeHandler handles dispatch when the inbound shape needs
// non-trivial translation to the upstream wire format. Each adapter that
// owns an inbound shape with cross-shape semantics (e.g. openai_responses
// against non-OpenAI hosts) registers one. Dispatch routes to it instead
// of the byte-pass path. Keeps inference shape-agnostic — it only knows
// "is there a handler for this inbound shape" without importing the
// adapter packages.
type CrossShapeHandler = func(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput, plan *routing.Plan)

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

	// Pipeline orchestrates one normal-mode inference request.
	Pipeline *pipeline.Pipeline

	// Proxy orchestrates a proxy-mode (BYO upstream key) request.
	Proxy *proxy.Pipeline

	// Adapters keys the wire-protocol implementation by adapters.Name.
	// Handlers look up the binding's Adapter Name here at request time;
	// proxy mode looks up the extractor by inbound endpoint shape.
	Adapters map[adapters.Name]pipeline.Adapter

	// Translators keys the shape Translator by adapters.Name. Each route
	// handler picks its own inbound translator (by route) and the
	// upstream translator (from plan.HostBinding.Adapter) and chains
	// them. Identity (OpenAI) is no-op so passthrough stays cheap.
	Translators adapters.Registry

	// RouteMounters are per-adapter route registration functions. Each
	// adapter package exposes a MountRoutes that satisfies RouteMounter;
	// cmd/relay/main.go wires them in. Order is iteration order over the
	// slice; should not matter in practice (paths are distinct).
	RouteMounters []RouteMounter

	// CrossShapeHandlers keys per-inbound-shape cross-shape dispatch by
	// adapters.Name. Populated by the composition root from each adapter
	// package that owns one. Dispatch consults this when the inbound shape
	// can't byte-pass to the resolved upstream.
	CrossShapeHandlers map[adapters.Name]CrossShapeHandler
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
	cfg.DocsPath = ""
	r.Get("/docs", httpapi.ScalarHandler("Wyolet Relay — Inference", "/openapi.json"))

	api := humachi.New(r, cfg)

	// /healthz is public.
	registerHealth(api, d)

	// /v1/* operations classify the request mode first, then conditionally
	// auth: normal + proxy-authed need a relay key; proxy-anonymous skips.
	// The readiness gate runs ahead of both — until the catalog has
	// completed its first Reload, /v1/* returns 503 instead of an
	// empty-snapshot 404. /healthz stays unaffected.
	mw := huma.Middlewares{
		httpapi.HumaAuth(ReadinessMiddleware(d.Catalog)),
		httpapi.HumaAuth(ClassifyMiddleware()),
		httpapi.HumaAuth(RelayKeyAuthMiddleware(d.Catalog)),
	}
	for _, mount := range d.RouteMounters {
		mount(api, d, mw)
	}
	registerModels(api, d, mw)
	registerProxyHosts(api, d, mw)

	return api
}
