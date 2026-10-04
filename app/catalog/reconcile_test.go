package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// catalogFromFixture returns a *Catalog loaded with the standard fixture.
func catalogFromFixture(t *testing.T) *Catalog {
	t.Helper()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

func TestApply_UpsertNew(t *testing.T) {
	provID := meta.NewID()
	hostID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "new-prov", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "new-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://example.com"},
	}
	m := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-x",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "gpt-x-2025-01-01", OriginalName: "gpt-x-2025-01-01"}},
			Pointer:   "gpt-x-2025-01-01",
		},
	}
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-rl", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 10, Window: 60, Strategy: ratelimit.StrategyTokenBucket,
		}}},
	}
	hostTier := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-host-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-hk", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: hostTier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K_NEW"}},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-pol", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{
			ModelIDs:    []string{m.Meta.ID},
			HostKeyIDs:  []string{hk.Meta.ID},
			RateLimitID: rl.Meta.ID,
		},
	}

	// A create's row is committed to the store before its NOTIFY arrives, so
	// the listers must already hold the rows — the absent-id recovery path
	// re-Lists them; later upserts in the burst take the incremental path.
	c := New(provList{prov}, hostList{h}, polList{hostTier, pol}, modList{m}, keyList{hk}, rlList{rl}, rkList{}, rcList{}, bndList{})
	c.snap.Store(emptySnap())

	if err := c.ApplyProviderUpsert(prov); err != nil {
		t.Fatalf("ApplyProviderUpsert: %v", err)
	}
	if err := c.ApplyHostUpsert(h); err != nil {
		t.Fatalf("ApplyHostUpsert: %v", err)
	}
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("ApplyModelUpsert: %v", err)
	}
	if err := c.ApplyRateLimitUpsert(rl); err != nil {
		t.Fatalf("ApplyRateLimitUpsert: %v", err)
	}
	if err := c.ApplyPolicyUpsert(hostTier); err != nil {
		t.Fatalf("ApplyPolicyUpsert hostTier: %v", err)
	}
	if err := c.ApplyHostKeyUpsert(hk); err != nil {
		t.Fatalf("ApplyHostKeyUpsert: %v", err)
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

func TestDeindexModelSnapshots_PreservesSameNameOwnedByDifferentModel(t *testing.T) {
	provID := meta.NewID()
	snapshotName := "gpt-4o-2024-11-20"
	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	old := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "old-gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: snapshotName}}, Pointer: snapshotName},
	}
	current := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: snapshotName}}, Pointer: snapshotName},
	}
	s := Build([]*provider.Provider{prov}, nil, nil, nil, []*model.Model{current}, nil, nil, nil, nil)

	if got, _, ok := s.SnapshotByName(snapshotName); !ok || got.Meta.ID != current.Meta.ID {
		t.Fatalf("precondition SnapshotByName owner = %v ok=%v, want current model %q", got, ok, current.Meta.ID)
	}

	s.deindexModelSnapshots(old)

	got, _, ok := s.SnapshotByName(snapshotName)
	if !ok {
		t.Fatalf("SnapshotByName(%q) was removed by deindexing a different model", snapshotName)
	}
	if got.Meta.ID != current.Meta.ID {
		t.Fatalf("SnapshotByName(%q) owner = %q, want current model %q", snapshotName, got.Meta.ID, current.Meta.ID)
	}
}

func TestApply_DeleteModelKeepsMultiTargetPricingForSurvivingModel(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	hostID := hosts[0].Meta.ID
	goneID := models[0].Meta.ID
	survivorID := models[1].Meta.ID
	pr := &pricing.Pricing{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "multi-target-pr",
			Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID},
		},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{goneID, survivorID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1},
			},
		},
	}
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := c.Current().PriceByModelHost(goneID, hostID); !ok {
		t.Fatalf("pricing missing for model scheduled for delete")
	}
	if _, ok := c.Current().PriceByModelHost(survivorID, hostID); !ok {
		t.Fatalf("pricing missing for surviving model before delete")
	}

	if err := c.ApplyModelDelete(goneID); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}

	s := c.Current()
	if _, ok := s.Pricing(pr.Meta.ID); !ok {
		t.Fatalf("multi-target pricing %q was removed even though model %q still resolves", pr.Meta.ID, survivorID)
	}
	if _, ok := s.PriceByModelHost(goneID, hostID); ok {
		t.Fatalf("deleted model %q still has a pricing index entry", goneID)
	}
	got, ok := s.PriceByModelHost(survivorID, hostID)
	if !ok {
		t.Fatalf("surviving model %q lost its pricing index entry", survivorID)
	}
	if got.Meta.ID != pr.Meta.ID {
		t.Fatalf("surviving model pricing = %q, want %q", got.Meta.ID, pr.Meta.ID)
	}
}

func TestApply_DeleteCascadesToPricing(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
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
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{pr}, bnds)
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
	// An extra provider and model ride in the listers (rows land in the
	// store before their NOTIFY): upsert both, then delete the model.
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	provID := meta.NewID()
	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "extra-prov", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	m := &model.Model{
		Meta: meta.Metadata{
			ID:    meta.NewID(),
			Name:  "extra-model",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "extra-model-2025-01-01", OriginalName: "extra"}},
			Pointer:   "extra-model-2025-01-01",
		},
	}
	provs = append(provs, prov)
	models = append(models, m)
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if err := c.ApplyProviderUpsert(prov); err != nil {
		t.Fatalf("ApplyProviderUpsert: %v", err)
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
	for _, k := range s.keysByID {
		check(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundKeyRefs(k))
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
		keysByID:           map[string]*key.Key{},
		keysByHash:         map[string]*key.Key{},
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
