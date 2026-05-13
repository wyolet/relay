package catalog

import (
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
			Hosts:   []model.HostBinding{{HostID: hostID, UpstreamName: "gpt-4o"}},
			Aliases: []string{"openai/gpt-4o"},
		},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o-mini",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Hosts:   []model.HostBinding{{HostID: hostID, UpstreamName: "gpt-4o-mini"}},
			Aliases: []string{"openai/gpt-4o-mini", "openai/mini"},
		},
	}

	k1 := &hostkey.HostKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k1",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
		Spec: hostkey.Spec{
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K1"},
		},
	}
	k2 := &hostkey.HostKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k2",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
		Spec: hostkey.Spec{
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

	return provList{prov}, hostList{h}, polList{pol}, modList{m1, m2}, keyList{k1, k2}, rlList{rl}, rkList{rk}
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

func TestReload_DisabledPolicyEvictsReachables(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls
	// Disabling the policy strands the relaykey pointing at it; for this
	// test we also disable the relaykey so cross-entity validation passes
	// and we can verify the snapshot eviction behaviour in isolation.
	rks[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); ok {
		t.Error("disabled policy should be evicted")
	}
	if got := s.ModelsByName("openai/gpt-4o"); len(got) != 0 {
		t.Errorf("model with no enabled referrer should be evicted: got %d", len(got))
	}
	if got := s.ModelsByName("openai/gpt-4o-mini"); len(got) != 0 {
		t.Errorf("aliased model with no enabled referrer should be evicted: got %d", len(got))
	}
}

func TestReload_DisabledModelFailsValidation(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	models[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown or disabled model") {
		t.Fatalf("expected unknown-or-disabled-model error, got %v", err)
	}
}

func TestReload_RelayKeyToDisabledPolicyFails(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown or disabled policy") {
		t.Fatalf("expected unknown-or-disabled-policy error, got %v", err)
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
