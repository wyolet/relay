package catalog

import (
	"context"
	"github.com/wyolet/relay/pkg/adapters"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// In-memory listers; one each implements the narrow Lister interface
// catalog declares. No Postgres; tests run in microseconds.

type provList []*provider.Provider

func (l provList) List(context.Context) ([]*provider.Provider, error) { return l, nil }

type hostList []*host.Host

func (l hostList) List(context.Context) ([]*host.Host, error) { return l, nil }

type polList []*policy.Policy

func (l polList) List(context.Context) ([]*policy.Policy, error) { return l, nil }

type modList []*model.Model

func (l modList) List(context.Context) ([]*model.Model, error) { return l, nil }

type keyList []*hostkey.HostKey

func (l keyList) List(context.Context) ([]*hostkey.HostKey, error) { return l, nil }

type rlList []*ratelimit.RateLimit

func (l rlList) List(context.Context) ([]*ratelimit.RateLimit, error) { return l, nil }

type rkList []*key.Key

func (l rkList) List(context.Context) ([]*key.Key, error) { return l, nil }

type rcList []*pricing.Pricing

func (l rcList) List(context.Context) ([]*pricing.Pricing, error) { return l, nil }

type bndList []*binding.Binding

func (l bndList) List(context.Context) ([]*binding.Binding, error) { return l, nil }

// fixture builds a coherent set: 1 provider (vendor), 1 host (serving
// endpoint), 2 models served by that host, 2 keys for that host, 1
// ratelimit, 1 policy referencing all of those, 1 key pointing at
// the policy.
func fixture() (provList, hostList, polList, modList, keyList, rlList, rkList, bndList) {
	provID := meta.NewID()
	hostID := meta.NewID()

	m1 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "gpt-4o-2025-01-01", OriginalName: "gpt-4o-2025-01-01"}},
			Pointer:   "gpt-4o-2025-01-01",
		},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o-mini",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "gpt-4o-mini-2025-01-01", OriginalName: "gpt-4o-mini-2025-01-01"}},
			Pointer:   "gpt-4o-mini-2025-01-01",
		},
	}
	b1 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-4o-openai-direct", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m1.Meta.ID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	b2 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-4o-mini-openai-direct", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m2.Meta.ID, HostID: hostID, Adapter: adapters.OpenAI},
	}

	// Host-owned tier policy the hostkeys mirror.
	hostTier := &policy.Policy{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "openai-tier-default",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
	}

	k1 := &hostkey.HostKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k1",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
		Spec: hostkey.Spec{
			HostID:    hostID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K1"},
		},
	}
	k2 := &hostkey.HostKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k2",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
		Spec: hostkey.Spec{
			HostID:    hostID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K2"},
		},
	}

	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "rpm",
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 100, Window: 60, Strategy: ratelimit.StrategyTokenBucket,
		}}},
	}

	pol := &policy.Policy{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "cheap-tier",
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: policy.Spec{
			Models:     []string{"openai/gpt-4o", "openai/gpt-4o-mini"},
			PoolIDs:    []string{meta.NewID(), meta.NewID()},
			RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}},
		},
	}

	rk := &key.Key{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "cust-1",
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: key.Spec{
			KeyHash: strings.Repeat("a", 64),
			Prefix:  "rk_cust1",
		},
	}

	prov := &provider.Provider{
		Meta: meta.Metadata{
			ID: provID, Name: "openai",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
	}
	h := &host.Host{
		Meta: meta.Metadata{
			ID: hostID, Name: "openai-direct",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
		Spec: host.Spec{BaseURL: "https://api.openai.com"},
	}

	return provList{prov}, hostList{h}, polList{pol, hostTier}, modList{m1, m2}, keyList{k1, k2}, rlList{rl}, rkList{rk}, bndList{b1, b2}
}

func TestReload_HappyPath(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); !ok {
		t.Error("policy not in snapshot")
	}
	if got := s.ModelsByName("gpt-4o-mini"); len(got) != 1 {
		t.Errorf("model by slug: got %d matches, want 1", len(got))
	}
	if _, _, ok := s.SnapshotByName("gpt-4o-mini-2025-01-01"); !ok {
		t.Error("snapshot lookup failed")
	}
	if k, _ := s.KeyByHash(strings.Repeat("a", 64)); k == nil {
		t.Error("key hash lookup failed")
	}
	// The snapshot keeps a policy's links as stored; the plan builder
	// resolves them.
	pol, _ := s.PolicyByName("cheap-tier")
	if len(pol.Spec.PoolIDs) != 2 || len(pol.Spec.RateLimits) != 1 || len(pol.Spec.Models) != 2 {
		t.Errorf("policy spec = %+v, want its links kept", pol.Spec)
	}
}

// TestReload_DisabledPolicyDoesNotEvictModels verifies the enabled-only
// snapshot rule: disabling a Policy removes the Policy itself but its
// referenced Models / HostKeys / RateLimits stay (they're independently
// enabled). The reachability filter is gone.
func TestReload_DisabledPolicyDoesNotEvictModels(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls
	// Disabling the policy strands the key pointing at it; disable
	// the key too so cross-entity validation passes.
	rks[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); ok {
		t.Error("disabled policy should be evicted")
	}
	if got := s.ModelsByName("gpt-4o"); len(got) != 1 {
		t.Errorf("model should stay (independently enabled): got %d", len(got))
	}
	if got := s.ModelsByName("gpt-4o-mini"); len(got) != 1 {
		t.Errorf("second model should stay: got %d", len(got))
	}
}

// A host key no longer names a tier policy, so its host's tier policies can
// come and go without evicting it.
func TestReload_HostKeyOutlivesItsHostsPolicies(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, polList{pols[0]}, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(c.Current().AllHostKeys()); got != 2 {
		t.Fatalf("host keys = %d, want both kept", got)
	}
}

func TestReload_PricingResolves(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	hostID := hosts[0].Meta.ID
	modelID := models[0].Meta.ID
	pr := &pricing.Pricing{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "openai-standard",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{modelID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 2.50},
			},
		},
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := c.Current().PriceByModelHost(modelID, hostID)
	if !ok {
		t.Fatal("PriceByModelHost: not found")
	}
	if got.Meta.ID != pr.Meta.ID {
		t.Errorf("got pricing %q, want %q", got.Meta.ID, pr.Meta.ID)
	}
}

func TestReload_DuplicatePricingFails(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	hostID := hosts[0].Meta.ID
	modelID := models[0].Meta.ID
	mkPricing := func(name string) *pricing.Pricing {
		return &pricing.Pricing{
			Meta: meta.Metadata{
				ID:    meta.NewID(),
				Name:  name,
				Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
			},
			Spec: pricing.Spec{
				Currency:       "USD",
				TargetModelIDs: []string{modelID},
				Rates: []pricing.Rate{
					{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 3.00},
				},
			},
		}
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{mkPricing("p1"), mkPricing("p2")}, bnds)
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate pricing") {
		t.Fatalf("expected duplicate pricing error, got %v", err)
	}
}

// A HostKey whose host doesn't resolve is dropped from the snapshot (the ref
// is required to function).
func TestReload_HostKeyUnknownHostDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	keys[0].Spec.HostID = meta.NewID()
	bad := keys[0].Meta.ID

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	if _, ok := c.Current().HostKey(bad); ok {
		t.Errorf("hostkey with a dangling hostId should not be in snapshot")
	}
}
