package inference

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/proxy"
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
		IsNativePath: func(b *binding.Binding, h *host.Host) bool {
			return b.Spec.Adapter == adapters.OpenAI && h.Meta.Name == "openai"
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

// buildDeps wires the data plane over cat with a pipeline that counts nothing; a catalog from buildDispatchCatalog brings its plans too.
func buildDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	reg := buildTestRegistry()
	d := Deps{
		Catalog:  cat,
		Pipeline: &pipeline.Pipeline{Logger: nil},
		Proxy:    proxy.New(nil, nil, nil),
		Adapters: reg.AdapterMap(),
		Specs:    reg,
	}
	fixturesMu.Lock()
	if f, ok := fixtures[cat]; ok {
		d.Plans = f.plans.Holder
	}
	fixturesMu.Unlock()
	return d
}

// useSpecs swaps d's wire-shape registry, keeping the adapter map in step.
func useSpecs(d *Deps, reg *adapter.Registry) {
	d.Specs = reg
	d.Adapters = reg.AdapterMap()
}

// buildRunnableDeps is buildDeps with a fully-wired pipeline (Lua limiter and key pool over kv.Mem) so tests can complete a real upstream round-trip.
func buildRunnableDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	return buildRunnableDepsAt(t, cat, nil)
}

// buildRunnableDepsAt is buildRunnableDeps with the limiter reading clock (nil reads time.Now).
func buildRunnableDepsAt(t *testing.T, cat *catalog.Catalog, clock func() time.Time) Deps {
	t.Helper()
	d := buildDeps(t, cat)
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	lim := lua.New(pkgratelimit.New(mem, slog.Default(), clock))
	d.Pipeline = &pipeline.Pipeline{Limiter: lim, Selector: keypool.New(mem, slog.Default(), nil, nil), Logger: slog.Default()}
	d.Proxy = proxy.New(lim, nil, nil)
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
