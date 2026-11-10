package inference

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// anonymousHostDeps wires proxy mode with "test-model" on host "anthropic" at upstreamURL.
func anonymousHostDeps(tb testing.TB, upstreamURL string) Deps {
	tb.Helper()
	var w plantest.World
	prov := w.Provider("anthropic")
	h := w.Host("anthropic", upstreamURL)
	w.Bind(w.Model(prov, "test-model"), h, adapters.Anthropic)
	plans := w.Load(tb)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings}.Load(tb)
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	tb.Cleanup(func() { _ = mem.Close() })
	reg := buildTestRegistry()
	return Deps{Catalog: cat, Plans: plans.Holder, Proxy: proxy.New(lua.New(pkgratelimit.New(mem, slog.New(slog.DiscardHandler), nil)), nil, nil), Adapters: reg.AdapterMap(), Specs: reg}
}

// Anonymous proxy traffic names no caller and a host is no policy subject, so a pinned request goes through whatever its model, known to the catalog or not.
func TestProxyShape_AnonymousPinGoesThrough(t *testing.T) {
	for _, model := range []string{"test-model", "unknown-model"} {
		t.Run(model, func(t *testing.T) {
			up, got := recordingUpstream(t)
			d := anonymousHostDeps(t, up.URL)
			body := proxyTestBody(model, 16, false)
			w := serveProxyShape(d, proxyShapeCall{pin: "anthropic", body: bytes.NewReader(body), length: int64(len(body))})
			if w.Code != http.StatusOK || len(got) != 1 {
				t.Fatalf("status %d, upstream reached %d times: %s", w.Code, len(got), w.Body)
			}
		})
	}
}
