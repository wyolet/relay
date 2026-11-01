package inference

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// anonymousHostDeps wires proxy mode with "test-model" and "other-model" on host "anthropic" at upstreamURL, the host bound to a policy with spec when hostPolicy is non-nil.
func anonymousHostDeps(tb testing.TB, upstreamURL string, hostPolicy *policy.Spec) Deps {
	tb.Helper()
	var w plantest.World
	prov := w.Provider("anthropic")
	h := w.Host("anthropic", upstreamURL)
	for _, name := range []string{"test-model", "other-model"} {
		w.Bind(w.Model(prov, name), h, adapters.Anthropic)
	}
	if hostPolicy != nil {
		w.BindPolicy(w.Policy("host-rules", *hostPolicy), plantest.Subject(policybinding.SubjectHost, h.Meta.ID))
	}
	plans := w.Load(tb)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings}.Load(tb)
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	tb.Cleanup(func() { _ = mem.Close() })
	reg := buildTestRegistry()
	return Deps{Catalog: cat, Plans: plans.Holder, Proxy: proxy.New(lua.New(pkgratelimit.New(mem, slog.New(slog.DiscardHandler), nil)), nil, nil), Adapters: reg.AdapterMap(), Specs: reg}
}

// Anonymous proxy traffic names no caller, but a host it pins is still governed by the host's own policy: a disabled host policy refuses everything, a model list refuses the models outside it and those the catalog does not know, and without a host policy anything goes through.
func TestProxyShape_AnonymousPinTakesTheHostRung(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name   string
		policy *policy.Spec
		model  string
		want   int
		code   string
	}{
		{"no host policy", nil, "test-model", http.StatusOK, ""},
		{"no host policy, a model the catalog does not know", nil, "unknown-model", http.StatusOK, ""},
		{"disabled host policy", &policy.Spec{Enabled: &disabled}, "test-model", http.StatusForbidden, "policy_disabled"},
		{"a model the host allows", &policy.Spec{Models: []string{"anthropic/test-model"}}, "test-model", http.StatusOK, ""},
		{"a model the host denies", &policy.Spec{Models: []string{"anthropic/test-model"}}, "other-model", http.StatusForbidden, "model_not_allowed"},
		{"a model unknown to a host that names models", &policy.Spec{Models: []string{"anthropic/test-model"}}, "unknown-model", http.StatusForbidden, "model_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, got := recordingUpstream(t)
			d := anonymousHostDeps(t, up.URL, tc.policy)
			body := proxyTestBody(tc.model, 16, false)
			w := serveProxyShape(d, proxyShapeCall{pin: "anthropic", body: bytes.NewReader(body), length: int64(len(body))})
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status %d, want %d %s: %s", w.Code, tc.want, tc.code, w.Body)
			}
			reached := len(got) > 0
			if reached != (tc.want == http.StatusOK) {
				t.Fatalf("upstream reached = %v for status %d", reached, w.Code)
			}
		})
	}
}

// The host's own limits count anonymous traffic as well, without waiting for the model when they cover the whole host.
func TestProxyShape_AnonymousPinCountsTheHostLimit(t *testing.T) {
	up, got := recordingUpstream(t)
	var w plantest.World
	prov := w.Provider("anthropic")
	h := w.Host("anthropic", up.URL)
	w.Bind(w.Model(prov, "test-model"), h, adapters.Anthropic)
	rl := w.RateLimit(ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 1, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow})
	w.BindPolicy(w.Policy("host-cap", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), plantest.Subject(policybinding.SubjectHost, h.Meta.ID))
	plans := w.Load(t)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings}.Load(t)
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	reg := buildTestRegistry()
	d := Deps{Catalog: cat, Plans: plans.Holder, Proxy: proxy.New(lua.New(pkgratelimit.New(mem, slog.New(slog.DiscardHandler), nil)), nil, nil), Adapters: reg.AdapterMap(), Specs: reg}
	if root := plans.Root(); root.AtHostNeedsModel(h.Meta.ID) {
		t.Fatal("a whole-host limit made anonymous requests wait for their model")
	}

	body := proxyTestBody("test-model", 16, false)
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		w := serveProxyShape(d, proxyShapeCall{pin: "anthropic", body: bytes.NewReader(body), length: int64(len(body))})
		if w.Code != want {
			t.Fatalf("request %d: status %d, want %d: %s", i, w.Code, want, w.Body)
		}
	}
	if len(got) != 1 {
		t.Fatalf("upstream reached %d times, want once", len(got))
	}
}
