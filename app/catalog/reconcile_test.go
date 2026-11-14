package catalog

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/slug"
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
		Spec: hostkey.Spec{HostID: hostID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "K_NEW"}},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "new-pol", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{
			Models:     []string{"new-prov/gpt-x"},
			RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}},
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
	if _, ok := s.HostKey(hk.Meta.ID); !ok {
		t.Error("host key missing")
	}
}

// Each model write re-indexes every name the model answers to, in a new snapshot: the ones taken before keep resolving what they held. Disabling a model removes it like a delete; enabling it again brings it back.
func TestApply_ModelWritesReindexNamesCopyOnWrite(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	orig := models[0]
	// The store holds a row before its write is applied: an upsert of a model the snapshot lacks re-lists it.
	upsert := func(m *model.Model) error {
		models[0] = m
		return c.ApplyModelUpsert(m)
	}
	renamed := *orig
	renamed.Spec.Snapshots = []model.Snapshot{{Name: "gpt-4o-2026-02-02", OriginalName: "gpt-4o-2026-02-02"}}
	renamed.Spec.Pointer = "gpt-4o-2026-02-02"
	off := false
	disabled := renamed
	disabled.Spec.Enabled = &off

	const before, after = "gpt-4o-2025-01-01", "gpt-4o-2026-02-02"
	refs := []string{before, after, "openai/" + after, after + "@openai-direct", "gpt-4o-mini-2025-01-01"}
	gone := []string{"", "", "", "", "gpt-4o-mini"}
	present := []string{"", "gpt-4o", "gpt-4o", "gpt-4o", "gpt-4o-mini"}
	type step struct {
		name  string
		apply func() error
		want  []string // the model each ref resolves to, "" for a miss
		snap  *Snapshot
	}
	steps := []*step{
		{name: "as loaded", apply: func() error { return nil }, want: []string{"gpt-4o", "", "", "", "gpt-4o-mini"}},
		{name: "snapshot renamed", apply: func() error { return upsert(&renamed) }, want: present},
		{name: "disabled", apply: func() error { return upsert(&disabled) }, want: gone},
		{name: "enabled again", apply: func() error { return upsert(&renamed) }, want: present},
		{name: "deleted", apply: func() error { return c.ApplyModelDelete(orig.Meta.ID) }, want: gone},
	}
	for _, st := range steps {
		if err := st.apply(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		st.snap = c.Current()
	}
	// Checked after every write, so a write that reached into an earlier snapshot shows.
	for _, st := range steps {
		for i, ref := range refs {
			got := ""
			if m, _, _, ok := st.snap.ResolveSnapshot(slug.From(ref)); ok {
				got = m.Meta.Name
			}
			if got != st.want[i] {
				t.Errorf("%s: %s resolves to %q, want %q", st.name, ref, got, st.want[i])
			}
		}
		_, byID := st.snap.Model(orig.Meta.ID)
		wantModel := st.want[0] != "" || st.want[1] != ""
		if byName := len(st.snap.ModelsByName("gpt-4o")); byID != wantModel || byName != map[bool]int{true: 1}[wantModel] {
			t.Errorf("%s: model by id %v, %d by name; want present=%v", st.name, byID, byName, wantModel)
		}
	}
	if orig.Spec.Pointer != before || len(orig.Spec.Snapshots) != 1 || orig.Spec.Snapshots[0].Name != before {
		t.Errorf("the first snapshot's model row was modified: %+v", orig.Spec)
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

// A policy names models by ref, not by id, so deleting a model leaves the
// policy and its grant as they were.
func TestApply_DeleteModelKeepsThePolicy(t *testing.T) {
	c := catalogFromFixture(t)
	pol, _ := c.Current().PolicyByName("cheap-tier")
	var gone string
	for id := range c.Current().modelsByID {
		gone = id
		break
	}
	if err := c.ApplyModelDelete(gone); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}
	got, ok := c.Current().Policy(pol.Meta.ID)
	if !ok {
		t.Fatal("policy should survive a model delete")
	}
	if !slices.Equal(got.Spec.Models, pol.Spec.Models) || len(pol.Spec.Models) == 0 {
		t.Errorf("models = %v, want the grant %v kept as stored", got.Spec.Models, pol.Spec.Models)
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

	assertRefsSymmetric(t, c.Current(), "after upserting and deleting the extra model")
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
