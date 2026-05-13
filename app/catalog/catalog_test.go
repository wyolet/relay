package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/providerkey"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// In-memory listers; one each implements the narrow Lister interface
// catalog declares. No Postgres; tests run in microseconds.

type provList []*provider.Provider

func (l provList) List(context.Context) ([]*provider.Provider, error) { return l, nil }

type polList []*policy.Policy

func (l polList) List(context.Context) ([]*policy.Policy, error) { return l, nil }

type modList []*model.Model

func (l modList) List(context.Context) ([]*model.Model, error) { return l, nil }

type keyList []*providerkey.ProviderKey

func (l keyList) List(context.Context) ([]*providerkey.ProviderKey, error) { return l, nil }

type rlList []*ratelimit.RateLimit

func (l rlList) List(context.Context) ([]*ratelimit.RateLimit, error) { return l, nil }

type rkList []*relaykey.RelayKey

func (l rkList) List(context.Context) ([]*relaykey.RelayKey, error) { return l, nil }

// fixture builds a coherent set: 1 provider, 2 models, 2 keys, 1 ratelimit,
// 1 policy referencing all of those, 1 relaykey pointing at the policy.
func fixture() (provList, polList, modList, keyList, rlList, rkList) {
	provID := meta.NewID()

	m1 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{UpstreamName: "gpt-4o"},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o-mini",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{UpstreamName: "gpt-4o-mini", Aliases: []string{"mini"}},
	}

	k1 := &providerkey.ProviderKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k1",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: providerkey.Spec{
			ValueFrom: providerkey.ValueFrom{Kind: providerkey.ValueKindEnv, Env: "K1"},
		},
	}
	k2 := &providerkey.ProviderKey{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k2",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: providerkey.Spec{
			ValueFrom: providerkey.ValueFrom{Kind: providerkey.ValueKindEnv, Env: "K2"},
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
			ProviderKeyIDs: []string{k1.Meta.ID, k2.Meta.ID},
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
		Spec: provider.Spec{BaseURL: "https://api.openai.com"},
	}

	return provList{prov}, polList{pol}, modList{m1, m2}, keyList{k1, k2}, rlList{rl}, rkList{rk}
}

func TestReload_HappyPath(t *testing.T) {
	provs, pols, models, keys, rls, rks := fixture()
	c := New(provs, pols, models, keys, rls, rks)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); !ok {
		t.Error("policy not in snapshot")
	}
	if _, ok := s.ModelByName("openai/gpt-4o-mini"); !ok {
		t.Error("model not in snapshot")
	}
	if _, ok := s.ModelByName("openai/mini"); !ok {
		t.Error("alias lookup failed")
	}
	if _, ok := s.RelayKeyByHash(strings.Repeat("a", 64)); !ok {
		t.Error("relaykey hash lookup failed")
	}
	pol, _ := s.PolicyByName("cheap-tier")
	if got := len(s.ModelsInPolicy(pol.Meta.ID)); got != 2 {
		t.Errorf("models in policy: got %d, want 2", got)
	}
	if got := len(s.ProviderKeysInPolicy(pol.Meta.ID)); got != 2 {
		t.Errorf("keys in policy: got %d, want 2", got)
	}
	if s.RateLimitOfPolicy(pol.Meta.ID) == nil {
		t.Error("ratelimit not bound")
	}
}

func TestReload_DisabledPolicyEvictsReachables(t *testing.T) {
	provs, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls
	// Disabling the policy strands the relaykey pointing at it; for this
	// test we also disable the relaykey so cross-entity validation passes
	// and we can verify the snapshot eviction behaviour in isolation.
	rks[0].Spec.Enabled = &fls

	c := New(provs, pols, models, keys, rls, rks)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	if _, ok := s.PolicyByName("cheap-tier"); ok {
		t.Error("disabled policy should be evicted")
	}
	if _, ok := s.ModelByName("openai/gpt-4o"); ok {
		t.Error("model with no enabled referrer should be evicted")
	}
	if _, ok := s.ModelByName("openai/gpt-4o-mini"); ok {
		t.Error("aliased model with no enabled referrer should be evicted")
	}
}

func TestReload_DisabledModelFailsValidation(t *testing.T) {
	provs, pols, models, keys, rls, rks := fixture()
	fls := false
	models[0].Spec.Enabled = &fls

	c := New(provs, pols, models, keys, rls, rks)
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown or disabled model") {
		t.Fatalf("expected unknown-or-disabled-model error, got %v", err)
	}
}

func TestReload_RelayKeyToDisabledPolicyFails(t *testing.T) {
	provs, pols, models, keys, rls, rks := fixture()
	fls := false
	pols[0].Spec.Enabled = &fls

	c := New(provs, pols, models, keys, rls, rks)
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown or disabled policy") {
		t.Fatalf("expected unknown-or-disabled-policy error, got %v", err)
	}
}

func TestReload_AliasCollisionFails(t *testing.T) {
	provs, pols, models, keys, rls, rks := fixture()
	models[1].Spec.Aliases = []string{"gpt-4o"} // collides with models[0]'s name

	c := New(provs, pols, models, keys, rls, rks)
	err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected alias collision error, got %v", err)
	}
}
