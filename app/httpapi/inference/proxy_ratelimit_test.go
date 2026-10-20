package inference

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

const proxyClientIP = "203.0.113.7"

// proxyLimitCatalog holds the anthropic-adapter model "test-model" on host
// "anthropic" at upstreamURL, a policy "p" allowing policyPerHour requests,
// a passthrough key under it, and each system rate limit in systemPerHour.
func proxyLimitCatalog(t *testing.T, upstreamURL string, noAuth bool, policyPerHour int64, systemPerHour map[string]int64) (*catalog.Catalog, *Principal) {
	t.Helper()
	provID, hostID, hkID, modID, polID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	perHour := func(name string, amount int64) *ratelimit.RateLimit {
		return &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: ratelimit.MeterRequests, Amount: amount, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow,
			}}},
		}
	}
	polRL := perHour("policy-limit", policyPerHour)
	rls := []*ratelimit.RateLimit{polRL}
	for name, amount := range systemPerHour {
		rls = append(rls, perHour(name, amount))
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "proxyhash", PassthroughAllowed: true},
	}
	cat := catalogtest.Catalog{
		Providers: []*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		Hosts: []*host.Host{{
			Meta: meta.Metadata{ID: hostID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: upstreamURL, NoAuth: noAuth},
		}},
		HostKeys: []*hostkey.HostKey{{
			Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-dummy", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
		}},
		Models: []*model.Model{{
			Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
		}},
		Bindings: []*binding.Binding{{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.Anthropic},
		}},
		Policies: []*policy.Policy{{
			Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}, RateLimitID: polRL.Meta.ID},
		}},
		RateLimits: rls,
		Keys:       []*key.Key{k},
	}.Load(t)
	pol, ok := cat.Current().Policy(polID)
	if !ok {
		t.Fatal("policy missing from snapshot")
	}
	return cat, &Principal{CredentialKind: CredentialKey, CredentialID: k.Meta.ID, KeyHash: k.Spec.KeyHash, Key: k, Policy: pol, PassthroughAllowed: true}
}

// proxyLimitDeps wires deps whose proxy runner reserves through a policy
// service over a recording kv.Mem, so a test can count the scripts each
// request costs.
func proxyLimitDeps(t *testing.T, cat *catalog.Catalog) (Deps, *kvtest.Recording) {
	t.Helper()
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	store := kvtest.NewRecording(mem)
	limiter := pkgratelimit.New(store, slog.New(slog.DiscardHandler), nil)

	d := buildDeps(t, cat)
	d.Proxy = proxy.New(limiter, nil, nil)
	d.Proxy.Policy = policy.NewService(catSnapReader{cat: cat}, nil, limiter)
	return d, store
}

// sendProxy runs one proxy-mode request past the proxy-mode gate. A nil
// principal is anonymous; pin names the upstream-host header.
func sendProxy(d Deps, pr *Principal, pin, model string) (*httptest.ResponseRecorder, *lifecycle.Context) {
	mode := ModeProxyAnonymous
	if pr != nil {
		mode = ModeProxyAuthed
	}
	body := `{"model":"` + model + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	ctx := WithClassification(r.Context(), Classification{
		Mode: mode, Key: "relay-key", UpstreamAuth: "Bearer upstream-key", UpstreamHost: pin, ClientIP: proxyClientIP,
	})
	if pr != nil {
		ctx = withPrincipal(ctx, d.Catalog.Current(), pr, pr.Key)
	}
	lc := lifecycle.NewContext("req-proxy-limit", "proxy", time.Now())
	ctx = lifecycle.ContextWith(ctx, lc)
	w := httptest.NewRecorder()
	handleProxy(d, w, r.WithContext(ctx), adapters.Anthropic, model)
	return w, lc
}

// assertSameRateLimitResponse holds proxy mode's refusal to normal mode's:
// one condition, one 429 shape for a client to handle.
func assertSameRateLimitResponse(t *testing.T, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != http.StatusTooManyRequests || want.Code != http.StatusTooManyRequests {
		t.Fatalf("status: proxy %d, normal %d, want 429 on both; proxy body %s", got.Code, want.Code, got.Body)
	}
	for _, h := range []string{"Retry-After", "Content-Type", "X-Should-Retry", HeaderOrigin} {
		if g, w := got.Header().Get(h), want.Header().Get(h); g != w || g == "" {
			t.Errorf("%s: proxy %q, normal %q", h, g, w)
		}
	}
	ge, we := parseDispatchErr(t, got.Body.Bytes()), parseDispatchErr(t, want.Body.Bytes())
	if ge.Error.Type != we.Error.Type || ge.Error.Code != we.Error.Code || ge.Error.Code != "rate_limit_exceeded" {
		t.Errorf("envelope: proxy %s/%s, normal %s/%s", ge.Error.Type, ge.Error.Code, we.Error.Type, we.Error.Code)
	}
}

// normalModeRateLimited exhausts the same one-per-hour policy in normal mode
// and returns its refusal.
func normalModeRateLimited(t *testing.T, upstreamURL string) *httptest.ResponseRecorder {
	t.Helper()
	cat, pr := proxyLimitCatalog(t, upstreamURL, true, 1, nil)
	d := buildRunnableDeps(t, cat)
	send := func() *httptest.ResponseRecorder {
		r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil), pr)
		w := httptest.NewRecorder()
		Dispatch(d, w, r, DispatchInput{Inbound: adapters.Anthropic, Body: []byte(countBody), ModelName: "test-model"})
		return w
	}
	if w := send(); w.Code != http.StatusOK {
		t.Fatalf("normal mode first request: status %d body %s", w.Code, w.Body)
	}
	return send()
}

// A passthrough key is metered by its policy's rate limit in proxy mode too,
// on a host resolved from the policy.
func TestProxyAuthed_PolicyRateLimit_PolicyResolvedHost(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, pr := proxyLimitCatalog(t, up.URL, true, 1, nil)
	d, _ := proxyLimitDeps(t, cat)

	if w, _ := sendProxy(d, pr, "", "test-model"); w.Code != http.StatusOK {
		t.Fatalf("first: status %d body %s", w.Code, w.Body)
	}
	w, _ := sendProxy(d, pr, "", "test-model")
	if genHits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", genHits.Load())
	}
	assertSameRateLimitResponse(t, w, normalModeRateLimited(t, up.URL))
}

// A pinned host is metered by the policy whether or not the model routes
// there: a routed model selects the rate limit and stamps model identity, an
// unrouted one falls back to the policy's flat limit.
func TestProxyAuthed_PolicyRateLimit_PinnedHost(t *testing.T) {
	for _, tc := range []struct {
		name      string
		model     string
		wantModel bool
	}{
		{name: "model resolves at the pin", model: "test-model", wantModel: true},
		{name: "model does not resolve", model: "unknown-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var countHits, genHits atomic.Int32
			up := countUpstreamServer(t, &countHits, &genHits)
			cat, pr := proxyLimitCatalog(t, up.URL, true, 1, nil)
			d, _ := proxyLimitDeps(t, cat)

			w, lc := sendProxy(d, pr, "anthropic", tc.model)
			if w.Code != http.StatusOK {
				t.Fatalf("first: status %d body %s", w.Code, w.Body)
			}
			if got := lc.ModelID != ""; got != tc.wantModel {
				t.Fatalf("model identity stamped = %v (%q), want %v", got, lc.ModelName, tc.wantModel)
			}
			w, _ = sendProxy(d, pr, "anthropic", tc.model)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("second: status %d, want 429; body %s", w.Code, w.Body)
			}
			if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "rate_limit_exceeded" || w.Header().Get("Retry-After") == "" {
				t.Fatalf("second: code %q retry-after %q", e.Error.Code, w.Header().Get("Retry-After"))
			}
			if genHits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1", genHits.Load())
			}
		})
	}
}

// The system proxy bucket stays a ceiling: it rides the policy's single
// Reserve, under the policy's hash tag, and refuses when it is the tighter.
func TestProxyAuthed_SystemCeilingInTheSameReserve(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, pr := proxyLimitCatalog(t, up.URL, true, 100, map[string]int64{systemRLProxyAuthed: 1})
	d, store := proxyLimitDeps(t, cat)

	if w, _ := sendProxy(d, pr, "", "test-model"); w.Code != http.StatusOK {
		t.Fatalf("first: status %d body %s", w.Code, w.Body)
	}
	reserves := store.Named("limit.reserve")
	if len(reserves) != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", len(reserves))
	}
	var sawPolicy, sawCeiling bool
	for _, k := range reserves[0].Keys {
		if !strings.HasPrefix(k, "limit:{p}:") {
			t.Errorf("key %q is outside the policy hash tag", k)
		}
		sawPolicy = sawPolicy || strings.Contains(k, ":policy:p:")
		sawCeiling = sawCeiling || strings.Contains(k, ":proxy:"+pr.KeyHash+":")
	}
	if !sawPolicy || !sawCeiling {
		t.Fatalf("reserve keys = %v, want the policy rule and the system ceiling", reserves[0].Keys)
	}

	w, _ := sendProxy(d, pr, "", "test-model")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status %d, want the ceiling's 429; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); !strings.Contains(e.Error.Message, pr.KeyHash) {
		t.Fatalf("refusal %q does not name the system ceiling's rule", e.Error.Message)
	}
	if got := len(store.Named("limit.reserve")); got != 2 {
		t.Fatalf("reserve scripts after two requests = %d, want 2", got)
	}
}

// A revoked token is refused in proxy mode as in normal mode: the check rides
// the same Reserve. The credential middleware refuses tokens passthrough
// today; this holds the reservation to it regardless.
func TestProxyAuthed_RevokedTokenRefused(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, base := proxyLimitCatalog(t, up.URL, true, 100, nil)
	d, store := proxyLimitDeps(t, cat)
	pr := &Principal{CredentialKind: CredentialToken, CredentialID: "jti-1", TeamID: "team-1", Policy: base.Policy, PassthroughAllowed: true}

	if err := store.Set(context.Background(), policy.RevokedKey("team-1", "jti-1"), []byte("1"), time.Hour); err != nil {
		t.Fatalf("denylist: %v", err)
	}
	w, _ := sendProxy(d, pr, "", "test-model")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "token_revoked" {
		t.Fatalf("code %q, want token_revoked", e.Error.Code)
	}
	if genHits.Load() != 0 {
		t.Fatal("a revoked token reached the upstream")
	}
}

// Anonymous proxy traffic is metered by the per-IP system bucket alone.
func TestProxyAnonymous_SystemBucketOnly(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, _ := proxyLimitCatalog(t, up.URL, false, 100, map[string]int64{systemRLProxyAnon: 1})
	d, store := proxyLimitDeps(t, cat)

	if w, _ := sendProxy(d, nil, "anthropic", "test-model"); w.Code != http.StatusOK {
		t.Fatalf("first: status %d body %s", w.Code, w.Body)
	}
	if w, _ := sendProxy(d, nil, "anthropic", "test-model"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status %d, want 429; body %s", w.Code, w.Body)
	}
	reserves := store.Named("limit.reserve")
	if len(reserves) != 2 {
		t.Fatalf("reserve scripts = %d, want one per request", len(reserves))
	}
	for _, s := range reserves {
		for _, k := range s.Keys {
			if !strings.HasPrefix(k, "limit:{"+proxyClientIP+"}:") || !strings.Contains(k, ":proxy:"+proxyClientIP+":") {
				t.Errorf("anonymous reserve key %q, want only the per-IP system bucket", k)
			}
		}
	}
}
