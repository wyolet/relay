package catalog

import (
	"github.com/wyolet/relay/app/adapters"
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

// catalogFromFixture returns a *Catalog loaded with the standard fixture.
func catalogFromFixture(t *testing.T) *Catalog {
	t.Helper()
	provs, hosts, pols, models, keys, rls, rks := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

func TestApply_UpsertNew(t *testing.T) {
	c := New(provList{}, hostList{}, polList{}, modList{}, keyList{}, rlList{}, rkList{}, rcList{})
	c.snap.Store(emptySnap())

	provID := meta.NewID()
	hostID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "new-prov", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	if err := c.ApplyProviderUpsert(prov); err != nil {
		t.Fatalf("ApplyProviderUpsert: %v", err)
	}

	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "new-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://example.com"},
	}
	if err := c.ApplyHostUpsert(h); err != nil {
		t.Fatalf("ApplyHostUpsert: %v", err)
	}

	m := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-x",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Hosts:     []model.HostBinding{{HostID: hostID, Adapter: adapters.OpenAI}},
			Snapshots: []model.Snapshot{{Name: "gpt-x-2025-01-01", OriginalName: "gpt-x-2025-01-01"}},
			Pointer:   "gpt-x-2025-01-01",
		},
	}
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("ApplyModelUpsert: %v", err)
	}

	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-rl", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 10, Window: 60, Strategy: ratelimit.StrategyTokenBucket,
		}}},
	}
	if err := c.ApplyRateLimitUpsert(rl); err != nil {
		t.Fatalf("ApplyRateLimitUpsert: %v", err)
	}

	hostTier := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-host-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
	}
	if err := c.ApplyPolicyUpsert(hostTier); err != nil {
		t.Fatalf("ApplyPolicyUpsert hostTier: %v", err)
	}

	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-hk", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: hostTier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K_NEW"}},
	}
	if err := c.ApplyHostKeyUpsert(hk); err != nil {
		t.Fatalf("ApplyHostKeyUpsert: %v", err)
	}

	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-pol", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{
			ModelIDs:    []string{m.Meta.ID},
			HostKeyIDs:  []string{hk.Meta.ID},
			RateLimitID: rl.Meta.ID,
		},
	}
	if err := c.ApplyPolicyUpsert(pol); err != nil {
		t.Fatalf("ApplyPolicyUpsert: %v", err)
	}

	s := c.Current()
	if _, ok := s.Provider(provID); !ok {
		t.Error("provider missing")
	}
	if _, ok := s.Host(hostID); !ok {
		t.Error("host missing")
	}
	if got := s.ModelsByName("gpt-x"); len(got) != 1 {
		t.Errorf("model by name: got %d, want 1", len(got))
	}
	if _, _, ok := s.SnapshotByName("gpt-x-2025-01-01"); !ok {
		t.Error("snapshot lookup failed")
	}
	if _, ok := s.Policy(pol.Meta.ID); !ok {
		t.Error("policy missing")
	}
	if got := len(s.ModelsInPolicy(pol.Meta.ID)); got != 1 {
		t.Errorf("ModelsInPolicy: got %d, want 1", got)
	}
}

func TestApply_UpsertExisting(t *testing.T) {
	c := catalogFromFixture(t)
	s0 := c.Current()

	// Grab fixture model[0]; add a new alias and change its name.
	var orig *model.Model
	for _, m := range s0.modelsByID {
		orig = m
		break
	}

	updated := &model.Model{
		Meta: orig.Meta, // same ID
		Spec: model.Spec{
			Hosts:     orig.Spec.Hosts,
			Snapshots: orig.Spec.Snapshots,
			Pointer:   orig.Spec.Pointer,
		},
	}

	if err := c.ApplyModelUpsert(updated); err != nil {
		t.Fatalf("ApplyModelUpsert: %v", err)
	}
	s := c.Current()

	// Model still resolvable by its own name after upsert.
	if got := s.ModelsByName(orig.Meta.Name); len(got) != 1 {
		t.Errorf("model name: got %d, want 1", len(got))
	}
}

func TestApply_DeleteCascadesToPricing(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks := fixture()
	hostID := hosts[0].Meta.ID
	modelID := models[0].Meta.ID
	pr := &pricing.Pricing{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "cascade-pr",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{modelID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1},
			},
		},
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Sanity: pricing present before delete.
	if _, ok := c.Current().Pricing(pr.Meta.ID); !ok {
		t.Fatal("pricing missing before delete")
	}

	if err := c.ApplyModelDelete(modelID); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}

	s := c.Current()
	if _, ok := s.Model(modelID); ok {
		t.Error("model should be gone")
	}
	if _, ok := s.Pricing(pr.Meta.ID); ok {
		t.Error("pricing should have been cascade-deleted")
	}
}

// Deleting a Model used to cascade-delete its referencing Policy. Now the
// Policy survives with the dead modelID silently stripped from its
// snapshot Spec — PG retains the original list.
func TestApply_DeleteModelStripsFromPolicy(t *testing.T) {
	c := catalogFromFixture(t)
	s0 := c.Current()

	pol, _ := s0.PolicyByName("cheap-tier")
	models := s0.ModelsInPolicy(pol.Meta.ID)
	if len(models) == 0 {
		t.Fatal("no models in policy")
	}
	gone := models[0].Meta.ID

	if err := c.ApplyModelDelete(gone); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}

	s := c.Current()
	got, ok := s.Policy(pol.Meta.ID)
	if !ok {
		t.Fatal("policy should survive a soft-ref parent delete")
	}
	for _, id := range got.Spec.ModelIDs {
		if id == gone {
			t.Errorf("deleted model %q still in policy.Spec.ModelIDs", gone)
		}
	}
}

func TestApply_DisableEqualsDelete(t *testing.T) {
	c := catalogFromFixture(t)
	s0 := c.Current()

	var m *model.Model
	for _, m2 := range s0.modelsByID {
		m = m2
		break
	}

	fls := false
	disabled := &model.Model{Meta: m.Meta, Spec: m.Spec}
	disabled.Spec.Enabled = &fls

	if err := c.ApplyModelUpsert(disabled); err != nil {
		t.Fatalf("ApplyModelUpsert (disabled): %v", err)
	}

	s := c.Current()
	if _, ok := s.Model(m.Meta.ID); ok {
		t.Error("disabled model should be absent (treated as delete)")
	}
}

func TestApply_ToggleTwice(t *testing.T) {
	c := catalogFromFixture(t)
	s0 := c.Current()

	var m *model.Model
	for _, m2 := range s0.modelsByID {
		m = m2
		break
	}

	// Delete.
	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := c.Current().Model(m.Meta.ID); ok {
		t.Error("should be gone after delete")
	}

	// Re-upsert — need enabled flag unset.
	restored := &model.Model{Meta: m.Meta, Spec: m.Spec}
	restored.Spec.Enabled = nil
	if err := c.ApplyModelUpsert(restored); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	if _, ok := c.Current().Model(m.Meta.ID); !ok {
		t.Error("model should be back after re-upsert")
	}
}

func TestApply_RefInvariantsHold(t *testing.T) {
	c := catalogFromFixture(t)

	// Add a new provider and model, then delete the model.
	provID := meta.NewID()
	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "extra-prov", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	if err := c.ApplyProviderUpsert(prov); err != nil {
		t.Fatalf("ApplyProviderUpsert: %v", err)
	}

	hostID := c.Current().hostsByID
	var firstHostID string
	for id := range hostID {
		firstHostID = id
		break
	}

	m := &model.Model{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "extra-model",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Hosts:     []model.HostBinding{{HostID: firstHostID, Adapter: adapters.OpenAI}},
			Snapshots: []model.Snapshot{{Name: "extra-model-2025-01-01", OriginalName: "extra"}},
			Pointer:   "extra-model-2025-01-01",
		},
	}
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("ApplyModelUpsert: %v", err)
	}
	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}

	s := c.Current()

	// Invariant 1: every outbound ref resolves.
	check := func(child refKey, parents []refKey) {
		for _, p := range parents {
			if !s.rowExists(p) {
				t.Errorf("invariant: %s/%s -> %s/%s: parent missing", child.Kind, child.ID, p.Kind, p.ID)
			}
		}
	}
	for _, mm := range s.modelsByID {
		check(refKey{Kind: refModel, ID: mm.Meta.ID}, outboundModelRefs(mm))
	}
	for _, k := range s.hostKeysByID {
		check(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	}
	for _, p := range s.policiesByID {
		check(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	}
	for _, p := range s.pricingsByID {
		check(refKey{Kind: refPricing, ID: p.Meta.ID}, outboundPricingRefs(p))
	}
	for _, k := range s.relayKeysByID {
		check(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundRelayKeyRefs(k))
	}

	// Invariant 2: every dependent in refsBy* exists.
	checkMap := func(name string, mm map[string]refSet) {
		for parentID, set := range mm {
			for child := range set {
				if !s.rowExists(child) {
					t.Errorf("invariant: %s[%s]: dependent %s/%s not in snapshot",
						name, parentID, child.Kind, child.ID)
				}
			}
		}
	}
	checkMap("refsByProvider", s.refsByProvider)
	checkMap("refsByHost", s.refsByHost)
	checkMap("refsByModel", s.refsByModel)
	checkMap("refsByHostKey", s.refsByHostKey)
	checkMap("refsByRateLimit", s.refsByRateLimit)
	checkMap("refsByPolicy", s.refsByPolicy)
}

// emptySnap returns a Snapshot with all maps initialized but empty.
func emptySnap() *Snapshot {
	return &Snapshot{
		providersByID:      map[string]*provider.Provider{},
		providersByName:    map[string]*provider.Provider{},
		hostsByID:          map[string]*host.Host{},
		hostsByName:        map[string]*host.Host{},
		policiesByID:       map[string]*policy.Policy{},
		policiesByName:     map[string]*policy.Policy{},
		modelsByID:         map[string]*model.Model{},
		modelsByName:       map[string][]*model.Model{},
		hostKeysByID:       map[string]*hostkey.HostKey{},
		rateLimitsByID:     map[string]*ratelimit.RateLimit{},
		relayKeysByID:      map[string]*relaykey.RelayKey{},
		relayKeysByHash:    map[string]*relaykey.RelayKey{},
		modelsByPolicy:     map[string][]*model.Model{},
		hostKeysByPolicy:   map[string][]*hostkey.HostKey{},
		rateLimitByPolicy:  map[string]*ratelimit.RateLimit{},
		pricingsByID:       map[string]*pricing.Pricing{},
		pricingByModelHost: map[string]*pricing.Pricing{},
		refsByProvider:     map[string]refSet{},
		refsByHost:         map[string]refSet{},
		refsByModel:        map[string]refSet{},
		refsByHostKey:      map[string]refSet{},
		refsByRateLimit:    map[string]refSet{},
		refsByPolicy:       map[string]refSet{},
	}
}

// keep test-only import from being trimmed
var _ = strings.Contains
