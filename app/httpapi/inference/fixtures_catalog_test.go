package inference

import (
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
)

// dispatchFixture is the plans behind one buildDispatchCatalog catalog, so a later edit (pointHostAt) reaches the principal the test already holds.
type dispatchFixture struct {
	plans     *plantest.Plans
	host      *host.Host
	key       *key.Key
	principal *Principal
}

var (
	fixturesMu sync.Mutex
	fixtures   = map[*catalog.Catalog]*dispatchFixture{}
)

func fixtureOf(t *testing.T, cat *catalog.Catalog) *dispatchFixture {
	t.Helper()
	fixturesMu.Lock()
	defer fixturesMu.Unlock()
	f, ok := fixtures[cat]
	if !ok {
		t.Fatal("catalog was not built by buildDispatchCatalog")
	}
	return f
}

// buildDispatchCatalog builds plans with a model "test-model" bound to the host named hostName with the provided adapter, reachable through a system-owned credential, and a user's key. It returns a catalog over the same catalog rows and the key's resolved principal. An optional Capabilities value is applied to the model.
func buildDispatchCatalog(t *testing.T, hostName string, hostAdapter adapters.Name, caps ...model.Capabilities) (*catalog.Catalog, *Principal) {
	t.Helper()
	var w plantest.World
	prov := w.Provider(hostName)
	h := w.Host(hostName, "http://upstream.invalid")
	m := w.Model(prov, "test-model")
	if len(caps) > 0 {
		m.Spec.Capabilities = caps[0]
	}
	w.Bind(m, h, hostAdapter)
	w.Credential(h, nil, plantest.System, "sk-test")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	k.Meta.Name = "rk"
	plans := w.Load(t)

	f := &dispatchFixture{plans: plans, host: h, key: k}
	f.principal = f.resolve(t)

	cat := catalogtest.Catalog{
		Providers: []*provider.Provider{prov},
		Hosts:     []*host.Host{h},
		Models:    []*model.Model{m},
		HostKeys:  w.Credentials,
		Keys:      []*key.Key{k},
		Bindings:  []*binding.Binding{w.Bindings[0]},
	}.Load(t)
	fixturesMu.Lock()
	fixtures[cat] = f
	fixturesMu.Unlock()
	t.Cleanup(func() {
		fixturesMu.Lock()
		delete(fixtures, cat)
		fixturesMu.Unlock()
	})
	return cat, f.principal
}

// applyModel writes m to a buildDispatchCatalog catalog's plans and catalog, and refreshes the principal the test holds.
func applyModel(t *testing.T, cat *catalog.Catalog, m *model.Model) error {
	t.Helper()
	f := fixtureOf(t, cat)
	plantest.Apply(t, f.plans, m)
	*f.principal = *f.resolve(t)
	return cat.ApplyModelUpsert(m)
}

// limitCaller binds a policy capping the fixture user at perHour requests an hour, and refreshes the principal the test holds.
func limitCaller(t *testing.T, cat *catalog.Catalog, perHour int64) {
	t.Helper()
	f := fixtureOf(t, cat)
	var w plantest.World
	rl := w.RateLimit(ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: perHour, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow})
	pol := w.Policy("hourly", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	pb := w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, f.principal.UserID))
	plantest.Apply(t, f.plans, rl)
	plantest.Apply(t, f.plans, pol)
	plantest.Apply(t, f.plans, pb)
	*f.principal = *f.resolve(t)
}

// resolve is the principal the fixture's key authenticates as now.
func (f *dispatchFixture) resolve(t *testing.T) *Principal {
	t.Helper()
	kp := f.plans.Root().Keys.Lookup(f.key.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatal("fixture key has no plan")
	}
	return principalOf(kp, f.key.Spec.KeyHash)
}

// pointHostAt re-points the fixture's host at upstreamURL in the plans and the catalog, and refreshes the principal the test holds in place, so a request reaches a test server.
func pointHostAt(t *testing.T, cat *catalog.Catalog, upstreamURL string) {
	t.Helper()
	f := fixtureOf(t, cat)
	h := *f.host
	h.Spec = host.Spec{BaseURL: upstreamURL}
	plantest.Apply(t, f.plans, &h)
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	f.host = &h
	*f.principal = *f.resolve(t)
}
