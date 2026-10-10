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
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

const countBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

// meteredCountCatalog is an anthropic-adapter catalog on a keyless host at upstreamURL whose only policy allows one request per hour.
func meteredCountCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	provID, hostID, hkID, modID, polID, rlID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstreamURL, NoAuth: true},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-dummy", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.Anthropic},
	}
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: rlID, Name: "one-per-hour", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 1, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow,
		}}},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}, RateLimitID: rlID},
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "meteredhash"},
	}
	cat := catalogtest.Catalog{
		Providers:  []*provider.Provider{prov},
		Hosts:      []*host.Host{h},
		Policies:   []*policy.Policy{pol},
		Models:     []*model.Model{m},
		HostKeys:   []*hostkey.HostKey{hk},
		RateLimits: []*ratelimit.RateLimit{rl},
		Keys:       []*key.Key{k},
		Bindings:   []*binding.Binding{b},
	}.Load(t)
	snapPol, ok := cat.Current().Policy(polID)
	if !ok {
		t.Fatal("policy missing from snapshot")
	}
	return cat, &Principal{CredentialKind: CredentialKey, CredentialID: k.Meta.ID, KeyHash: k.Spec.KeyHash, Key: k, Policy: snapPol}
}

func countingDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, countingRegistry())
	d.Profiles = profileRegistry(newCountProfile())
	return d
}

func generate(d Deps, pr *Principal) *httptest.ResponseRecorder {
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil), pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{Inbound: adapters.Anthropic, Body: []byte(countBody), ModelName: "test-model"})
	return w
}

// countUpstream answers the counting endpoint and the generation endpoint, counting hits on each.
func countUpstreamServer(t *testing.T, countHits, genHits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			countHits.Add(1)
			_, _ = w.Write([]byte(`{"input_tokens":12}`))
		case "/v1/messages":
			genHits.Add(1)
			_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// An upstream count spends the operator's credential, so it is metered by the caller's policy like any other upstream call.
func TestCountTokens_PolicyMeterBoundsUpstreamCounts(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, pr := meteredCountCatalog(t, up.URL)
	d := countingDeps(t, cat)

	if w := countRequest(d, pr, countBody, nil); w.Code != http.StatusOK {
		t.Fatalf("first count: status %d body %s", w.Code, w.Body)
	}
	w := countRequest(d, pr, countBody, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second count under an exhausted policy: status %d, want 429; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error code = %q, want rate_limit_exceeded", e.Error.Code)
	}
	if countHits.Load() != 1 {
		t.Fatalf("upstream count hits = %d, want 1", countHits.Load())
	}
	if g := generate(d, pr); g.Code != http.StatusTooManyRequests {
		t.Fatalf("generation after the count: status %d, want 429", g.Code)
	}
	if genHits.Load() != 0 {
		t.Fatalf("upstream generation hits = %d, want 0", genHits.Load())
	}
}

// The in-flight cap that sheds generations sheds counts too, before any upstream call.
func TestCountTokens_ShedAtTheInflightCap(t *testing.T) {
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)
	cat, pr := countCatalog(t, up.URL)
	d := countingDeps(t, cat)

	adm := httpapi.NewAdmission(1)
	reg := lifecycle.New()
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)
	d.Lifecycle = reg
	d.Pipeline.Lifecycle = reg

	// A free slot is taken and handed back: two counts in a row both pass.
	for i := range 2 {
		if w := countRequest(d, pr, countBody, nil); w.Code != http.StatusOK {
			t.Fatalf("count %d with a free slot: status %d body %s", i, w.Code, w.Body)
		}
		waitForFreeSlot(t, adm)
	}

	filler := lifecycle.NewContext("filler", "pipeline", time.Now())
	if err := adm.PreFlight(context.Background(), filler, &lifecycle.PreFlightEvent{}); err != nil {
		t.Fatalf("fill the slot: %v", err)
	}
	before := countHits.Load()
	w := countRequest(d, pr, countBody, nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != httpapi.RetryAfterShed {
		t.Fatalf("count at the cap: status %d retry-after %q, want 429/%q", w.Code, w.Header().Get("Retry-After"), httpapi.RetryAfterShed)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "overloaded" {
		t.Errorf("error code = %q, want overloaded", e.Error.Code)
	}
	if countHits.Load() != before {
		t.Fatal("a shed count still reached the upstream")
	}
}

// waitForFreeSlot waits for the detached post-flight to hand the admission slot back.
func waitForFreeSlot(t *testing.T, adm *httpapi.Admission) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		probe := lifecycle.NewContext("probe", "pipeline", time.Now())
		if adm.PreFlight(context.Background(), probe, &lifecycle.PreFlightEvent{}) == nil {
			adm.Collect(probe)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("admission slot never released")
}

// A token revoked by jti is refused on the count route as on every other inference route, on the upstream tier and on the local estimate alike.
func TestCountTokens_RevokedTokenRefused(t *testing.T) {
	ctx := context.Background()
	var countHits, genHits atomic.Int32
	up := countUpstreamServer(t, &countHits, &genHits)

	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	pol, ok := cat.Current().PolicyByName("p")
	if !ok {
		t.Fatal("fixture policy missing")
	}
	f := newPrincipalFixture()
	cat.UseTenancy(
		catalogtest.Rows[team.Team]{f.team}, catalogtest.Rows[project.Project]{f.project},
		catalogtest.Rows[serviceaccount.ServiceAccount]{f.sa}, catalogtest.Rows[group.Group]{f.group},
		catalogtest.Rows[role.Role]{}, catalogtest.Rows[rolebinding.RoleBinding]{},
		catalogtest.Rows[policybinding.PolicyBinding]{boundTo(f, "bind-user", 10, pol.Meta.ID, "user:"+f.user)},
	)
	cat.UseTokenVersions(f.versions)
	if err := cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	pointHostAt(t, cat, up.URL)

	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	svc := policy.NewService(catSnapReader{cat: cat}, keypool.New(mem, slog.Default(), nil, nil), pkgratelimit.New(mem, slog.Default(), nil))
	exact := buildDeps(t, cat)
	exact.Pipeline = &pipeline.Pipeline{Policy: svc, Logger: slog.Default()}
	exact.Tokens = f.tokens
	useSpecs(&exact, countingRegistry())
	exact.Profiles = profileRegistry(newCountProfile())
	estimate := exact
	useSpecs(&estimate, buildTestRegistry()) // no counting endpoint: the answer is local

	jti := ""
	tok := f.mint(t, func(c *crypto.TokenClaims) { jti = c.Jti })
	send := func(d Deps) *httptest.ResponseRecorder {
		handler := ClassifyMiddleware()(PrincipalMiddleware(cat, f.tokens)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleCountTokens(d, w, r.WithContext(clientprofile.WithProfile(r.Context(), newCountProfile())))
		})))
		req := httptest.NewRequest(http.MethodPost, "/counting/v1/messages/count_tokens", strings.NewReader(countBody))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(exact); rec.Code != http.StatusOK || countHits.Load() != 1 {
		t.Fatalf("before revocation: status=%d upstream=%d body=%s", rec.Code, countHits.Load(), rec.Body)
	}
	if rec := send(estimate); rec.Code != http.StatusOK {
		t.Fatalf("estimate before revocation: status=%d body=%s", rec.Code, rec.Body)
	}

	if err := mem.Set(ctx, policy.RevokedKey(f.team.Meta.ID, jti), []byte("1"), time.Hour); err != nil {
		t.Fatalf("denylist: %v", err)
	}
	for name, d := range map[string]Deps{"exact": exact, "estimate": estimate} {
		rec := send(d)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s after revocation: status=%d, want 401; body=%s", name, rec.Code, rec.Body)
		}
		if e := parseDispatchErr(t, rec.Body.Bytes()); e.Error.Code != "token_revoked" {
			t.Errorf("%s: error code = %q, want token_revoked", name, e.Error.Code)
		}
	}
	if countHits.Load() != 1 {
		t.Fatalf("a revoked token reached the upstream counter: hits=%d", countHits.Load())
	}
}
