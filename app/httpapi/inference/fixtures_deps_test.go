package inference

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

// stubV1Translator is a no-op v1.Translator for test specs.
type stubV1Translator struct{}

func (stubV1Translator) ParseRequest(body []byte) (*pkgrelay.Request, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) SerializeRequest(req *pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) ParseResponse(body []byte) (*pkgrelay.Response, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) SerializeResponse(resp *pkgrelay.Response, req *pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("stub: not implemented")
}
func (stubV1Translator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (stubV1Translator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

// buildTestRegistry constructs a minimal adapter.Registry for tests.
// Registers specs for openai, openai_responses, openai_embeddings, and
// anthropic — each with a stub translator so tests exercise dispatch routing
// without a live upstream.
func buildTestRegistry() *adapter.Registry {
	openaiSpec := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  stubV1Translator{},
		ParamPaths:  map[string]string{"temperature": "temperature", "top_p": "top_p"},
	}).Build()

	// Responses: IsNativePath returns true only when host name == "openai".
	responsesSpec := (&adapter.Spec{
		Name:        adapters.OpenAIResponses,
		DefaultPath: "/v1/responses",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  stubV1Translator{},
		IsNativePath: func(plan *routing.Plan) bool {
			return plan.HostBinding.Spec.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
		},
	}).Build()

	embeddingsSpec := (&adapter.Spec{
		Name:        adapters.OpenAIEmbeddings,
		DefaultPath: "/v1/embeddings",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		BytePass:    true,
	}).Build()

	anthropicSpec := (&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		Auth:        adapter.AuthStrategy{Header: "x-api-key"},
		Translator:  stubV1Translator{},
	}).Build()

	// Canonical inbound shape — real identity translator (not a stub).
	canonicalSpec := (&adapter.Spec{
		Name:       adapters.Canonical,
		Translator: pkgrelay.IdentityTranslator{},
	}).Build()

	return adapter.NewRegistry(openaiSpec, responsesSpec, embeddingsSpec, anthropicSpec, canonicalSpec)
}

func buildDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	limiter := pkgratelimit.New(kvStore, nil, nil)
	pl := &pipeline.Pipeline{Logger: nil}

	reg := buildTestRegistry()

	return Deps{
		Catalog:  cat,
		Resolver: routing.New(cat),
		Pipeline: pl,
		Proxy:    proxy.New(limiter, nil, nil),
		Adapters: reg.AdapterMap(),
		Specs:    reg,
	}
}

// useSpecs swaps d's wire-shape registry, keeping the adapter map in step.
func useSpecs(d *Deps, reg *adapter.Registry) {
	d.Specs = reg
	d.Adapters = reg.AdapterMap()
}

// catSnapReader adapts *catalog.Catalog to policy.SnapshotReader (mirrors
// cmd/relay's catalogSnapReader, which lives in the composition root).
type catSnapReader struct{ cat *catalog.Catalog }

func (r catSnapReader) Policy(_ context.Context, id string) (*policy.Policy, bool) {
	return r.cat.Current().Policy(id)
}
func (r catSnapReader) RateLimit(_ context.Context, id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
}

// buildRunnableDeps is buildDeps with a fully-wired pipeline (policy service
// over kv.Mem) so tests can complete a real upstream round-trip.
func buildRunnableDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	d := buildDeps(t, cat)
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	svc := policy.NewService(catSnapReader{cat: cat}, keypool.New(mem, slog.Default(), nil, nil), pkgratelimit.New(mem, slog.Default(), nil))
	d.Pipeline = &pipeline.Pipeline{Policy: svc, Logger: slog.Default()}
	return d
}

// withNormalContext injects a ModeNormal classification and relay key into
// r's context, simulating what the classifier + auth middleware would do.
func withNormalContext(r *http.Request, p *Principal) *http.Request {
	ctx := WithClassification(r.Context(), Classification{Mode: ModeNormal})
	ctx = context.WithValue(ctx, ctxKeyT{}, p.Key)
	ctx = context.WithValue(ctx, ctxPrincipalT{}, p)
	return r.WithContext(ctx)
}
