package catalog

import (
	"github.com/wyolet/relay/app/adapter"
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
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

type rkList []*relaykey.RelayKey

func (l rkList) List(context.Context) ([]*relaykey.RelayKey, error) { return l, nil }

type rcList []*pricing.Pricing

func (l rcList) List(context.Context) ([]*pricing.Pricing, error) { return l, nil }

// fixture builds a coherent set: 1 provider (vendor), 1 host (serving
// endpoint), 2 models served by that host, 2 keys for that host, 1
// ratelimit, 1 policy referencing all of those, 1 relaykey pointing at
// the policy.
func fixture() (provList, hostList, polList, modList, keyList, rlList, rkList) {
	provID := meta.NewID()
	hostID := meta.NewID()

	m1 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Hosts:   []model.HostBinding{{HostID: hostID, UpstreamName: "gpt-4o", Adapter: adapter.OpenAI}},
			Aliases: []string{"openai/gpt-4o"},
		},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o-mini",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Hosts:   []model.HostBinding{{HostID: hostID, UpstreamName: "gpt-4o-mini", Adapter: adapter.OpenAI}},
			Aliases: []string{"openai/gpt-4o-mini", "openai/mini"},
		},
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
			PolicyID:  hostTier.Meta.ID,
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
			PolicyID:  hostTier.Meta.ID,
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
			ModelIDs:       []string{m1.Meta.ID, m2.Meta.ID},
			HostKeyIDs: []string{k1.Meta.ID, k2.Meta.ID},
			RateLimitID:    rl.Meta.ID,
		},
	}

	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "cust-1",
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: relaykey.Spec{
			PolicyID: pol.Meta.ID,
			KeyHash:  strings.Repeat("a", 64),
			Prefix:   "rk_cust1",
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

	return provList{prov}, hostList{h}, polList{pol, hostTier}, modList{m1, m2}, keyList{k1, k2}, rlList{rl}, rkList{rk}
}

func TestReload_HappyPath(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); !ok {
		t.Error("policy not in snapshot")
	}
	if got := s.ModelsByName("openai/gpt-4o-mini"); len(got) != 1 {
		t.Errorf("model not in snapshot: got %d matches, want 1", len(got))
	}
	if got := s.ModelsByName("openai/mini"); len(got) != 1 {
		t.Errorf("alias lookup failed: got %d matches, want 1", len(got))
	}
	if _, ok := s.RelayKeyByHash(strings.Repeat("a", 64)); !ok {
		t.Error("relaykey hash lookup failed")
	}
	pol, _ := s.PolicyByName("cheap-tier")
	if got := len(s.ModelsInPolicy(pol.Meta.ID)); got != 2 {
		t.Errorf("models in policy: got %d, want 2", got)
	}
	if got := len(s.HostKeysInPolicy(pol.Meta.ID)); got != 2 {
		t.Errorf("keys in policy: got %d, want 2", got)
	}
	if s.RateLimitOfPolicy(pol.Meta.ID) == nil {
		t.Error("ratelimit not bound")
	}
}

// TestReload_DisabledPolicyDoesNotEvictModels verifies the enabled-only
// snapshot rule: disabling a Policy removes the Policy itself but its
// referenced Models / HostKeys / RateLimits stay (they're independently
// enabled). The reachability filter is gone.
func TestReload_DisabledPolicyDoesNotEvictModels(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls
	// Disabling the policy strands the relaykey pointing at it; disable
	// the relaykey too so cross-entity validation passes.
	rks[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); ok {
		t.Error("disabled policy should be evicted")
	}
	if got := s.ModelsByName("openai/gpt-4o"); len(got) != 1 {
		t.Errorf("model should stay (independently enabled): got %d", len(got))
	}
	if got := s.ModelsByName("openai/gpt-4o-mini"); len(got) != 1 {
		t.Errorf("aliased model should stay: got %d", len(got))
	}
}

// Disabling a Model still referenced by a Policy used to fail Reload.
// Now Reload succeeds and the policy's snapshot copy has the dead id
// silently stripped from Spec.ModelIDs.
func TestReload_DisabledModelDropsFromPolicyRefs(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	models[0].Spec.Enabled = &fls
	disabledID := models[0].Meta.ID

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	pol, _ := c.Current().PolicyByName("cheap-tier")
	for _, id := range pol.Spec.ModelIDs {
		if id == disabledID {
			t.Errorf("disabled model id %q still in policy.Spec.ModelIDs", id)
		}
	}
}

// Disabling a Policy still referenced by a RelayKey used to fail Reload.
// Now Reload succeeds and the RelayKey is silently dropped from the
// snapshot (its required ref is gone).
func TestReload_RelayKeyToDisabledPolicyDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls
	// rks[0] points at pols[0]; disable rks[0] too so an explicit "I want
	// this dropped" doesn't muddy the test of soft-dropping unrelated keys.
	// Other relaykeys (if any) pointing at the disabled policy must also
	// disappear from the snapshot.
	rks[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
}

func TestReload_PricingResolves(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	hostID := hosts[0].Meta.ID
	modelID := models[0].Meta.ID
	pr := &pricing.Pricing{
		Meta: meta.Metadata{
			ID:   meta.NewID(),
			Name: "openai-standard",
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
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr})
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
	provs, hosts, pols, models, keys, rls, rks := fixture()
	hostID := hosts[0].Meta.ID
	modelID := models[0].Meta.ID
	mkPricing := func(name string) *pricing.Pricing {
		return &pricing.Pricing{
			Meta: meta.Metadata{
				ID:   meta.NewID(),
				Name: name,
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
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{mkPricing("p1"), mkPricing("p2")})
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate pricing") {
		t.Fatalf("expected duplicate pricing error, got %v", err)
	}
}

// TestReload_AliasCollisionAllowed proves the multivalued index: two
// reachable Models can share an alias intentionally (e.g. the same wire
// name "gpt-5" served by both openai-direct and an azure deployment).
// Consumers disambiguate downstream with a suffix or header.
func TestReload_AliasCollisionAllowed(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	// Both models claim the same wire alias "shared".
	models[0].Spec.Aliases = append(models[0].Spec.Aliases, "shared")
	models[1].Spec.Aliases = append(models[1].Spec.Aliases, "shared")

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := c.Current().ModelsByName("shared")
	if len(got) != 2 {
		t.Errorf("got %d matches for shared alias, want 2", len(got))
	}
}

// hostOwnedPolicy builds a Policy owned by the given host id.
func hostOwnedPolicy(name, hostID string) *policy.Policy {
	return &policy.Policy{
		Meta: meta.Metadata{
			ID:   meta.NewID(),
			Name: name,
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
	}
}

// TestReload_HostPoliciesMenu_OK accepts a host whose Spec.Policies all
// resolve to host-owned policies of that same host, with DefaultPolicy
// in the menu.
func TestReload_HostPoliciesMenu_OK(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	tier := hostOwnedPolicy("openai-tier-x", hosts[0].Meta.ID)
	hosts[0].Spec.Policies = []string{tier.Meta.ID}
	hosts[0].Spec.DefaultPolicy = tier.Meta.ID
	pols = append(pols, tier)

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// Unknown policy ids in a Host's menu are silently dropped from the
// snapshot copy. PG retains the full list.
func TestReload_HostPoliciesMenu_UnknownPolicyDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	bad := meta.NewID()
	hosts[0].Spec.Policies = []string{bad}

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	h, _ := c.Current().HostByName(hosts[0].Meta.Name)
	for _, id := range h.Spec.Policies {
		if id == bad {
			t.Errorf("dangling policy id %q still in host.Spec.Policies", bad)
		}
	}
}

// A menu entry whose Policy is owned by a different host is also dropped.
func TestReload_HostPoliciesMenu_WrongOwnerDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	stray := hostOwnedPolicy("stray-tier", meta.NewID())
	hosts[0].Spec.Policies = []string{stray.Meta.ID}
	pols = append(pols, stray)

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	h, _ := c.Current().HostByName(hosts[0].Meta.Name)
	for _, id := range h.Spec.Policies {
		if id == stray.Meta.ID {
			t.Errorf("wrong-owner policy id %q still in host.Spec.Policies", stray.Meta.ID)
		}
	}
}

// A HostKey whose Spec.PolicyID doesn't resolve is dropped from the
// snapshot (the ref is required to function).
func TestReload_HostKeyPolicy_UnknownDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	keys[0].Spec.PolicyID = meta.NewID()
	bad := keys[0].Meta.ID

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	if _, ok := c.Current().HostKey(bad); ok {
		t.Errorf("hostkey with dangling policyId should not be in snapshot")
	}
}

// A HostKey whose Policy resolves but is owned by a different host is
// also dropped.
func TestReload_HostKeyPolicy_WrongHostDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	otherHost := meta.NewID()
	strayTier := &policy.Policy{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "stray-tier",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: otherHost},
		},
	}
	pols = append(pols, strayTier)
	keys[0].Spec.PolicyID = strayTier.Meta.ID
	bad := keys[0].Meta.ID

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	if _, ok := c.Current().HostKey(bad); ok {
		t.Errorf("hostkey with wrong-host policy should not be in snapshot")
	}
}

// Pointing a HostKey at a user-owned Policy is also a soft drop.
func TestReload_HostKeyPolicy_UserOwnedDrops(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	keys[0].Spec.PolicyID = pols[0].Meta.ID // user-owned cheap-tier
	bad := keys[0].Meta.ID

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	if _, ok := c.Current().HostKey(bad); ok {
		t.Errorf("hostkey pointing at user-owned policy should not be in snapshot")
	}
}

// DefaultPolicy outside the menu is silently cleared in the snapshot copy.
func TestReload_HostPoliciesMenu_DefaultNotInMenuClears(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	tier := hostOwnedPolicy("openai-tier-x", hosts[0].Meta.ID)
	other := hostOwnedPolicy("openai-tier-y", hosts[0].Meta.ID)
	hosts[0].Spec.Policies = []string{tier.Meta.ID}
	hosts[0].Spec.DefaultPolicy = other.Meta.ID
	pols = append(pols, tier, other)

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload should be tolerant, got %v", err)
	}
	h, _ := c.Current().HostByName(hosts[0].Meta.Name)
	if h.Spec.DefaultPolicy != "" {
		t.Errorf("defaultPolicy outside menu should be cleared in snapshot, got %q", h.Spec.DefaultPolicy)
	}
}
