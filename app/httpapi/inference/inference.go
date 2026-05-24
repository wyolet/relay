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

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/adapter"
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

	// Specs is the generic adapter registry. Dispatch uses it to look up
	// inbound and upstream translators and to determine routing strategy
	// (byte-pass vs canonical cross-shape). Built once at boot and
	// populated from cmd/relay/main.go.
	Specs *adapter.Registry

	// RouteMounters are per-adapter route registration functions. Each
	// adapter (or the generic framework) exposes a MountRoutes function
	// that satisfies RouteMounter; cmd/relay/main.go wires them in.
	RouteMounters []RouteMounter
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

	// /v1/ws is a raw upgrade, not a huma POST operation, so it mounts
	// directly on the chi router. It reuses the identical net/http
	// middleware chain (readiness → classify → relay-key auth), so the
	// upgrade request is authed exactly like an HTTP /v1/* request and
	// every multiplexed frame inherits the authed context.
	r.With(
		ReadinessMiddleware(d.Catalog),
		ClassifyMiddleware(),
		RelayKeyAuthMiddleware(d.Catalog),
	).Get("/v1/ws", wsHandler(d))

	return api
}
