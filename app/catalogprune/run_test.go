package catalogprune

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
)

// memTable holds one kind in memory and deletes like the Postgres stores: only
// while the row still has the version that was read.
type memTable[T any] struct {
	rows    []*T
	metaOf  func(*T) *meta.Metadata
	deleted []string
}

func (m *memTable[T]) List(context.Context) ([]*T, error) { return slices.Clone(m.rows), nil }

func (m *memTable[T]) DeleteAtVersion(_ context.Context, id, version string) error {
	for i, r := range m.rows {
		if md := m.metaOf(r); md.ID == id {
			if md.ResourceVersion != version {
				return meta.ErrStaleResourceVersion
			}
			m.rows = slices.Delete(m.rows, i, i+1)
			m.deleted = append(m.deleted, md.Name)
			return nil
		}
	}
	return meta.ErrStaleResourceVersion
}

// edit stands for an operator write: the row's version moves on.
func (m *memTable[T]) edit(name string) {
	for _, r := range m.rows {
		if md := m.metaOf(r); md.Name == name {
			v, _ := strconv.Atoi(md.ResourceVersion)
			md.ResourceVersion = strconv.Itoa(v + 1)
		}
	}
}

type memOverlays []*overlay.Overlay

func (o memOverlays) List(context.Context) ([]*overlay.Overlay, error) { return o, nil }

// usageLog answers the traffic guard from a fixed list of events.
type usageLog struct {
	events []usage.Event
	err    error
	reads  int
}

func (u *usageLog) Events(_ context.Context, q usage.EventQuery) ([]usage.Event, error) {
	u.reads++
	if u.err != nil {
		return nil, u.err
	}
	for i := len(u.events) - 1; i >= 0; i-- {
		e := u.events[i]
		if e.Timestamp.Before(q.From) || !slices.Contains(q.ModelID, e.ModelID) || (len(q.HostID) > 0 && !slices.Contains(q.HostID, e.HostID)) {
			continue
		}
		return []usage.Event{e}, nil
	}
	return nil, nil
}

// fixture is a catalog after a release dropped some rows, in memory.
type fixture struct {
	providers  *memTable[provider.Provider]
	hosts      *memTable[host.Host]
	models     *memTable[model.Model]
	pricings   *memTable[pricing.Pricing]
	bindings   *memTable[binding.Binding]
	policies   *memTable[policy.Policy]
	rateLimits *memTable[ratelimit.RateLimit]
	overlays   memOverlays
	usage      *usageLog
}

var (
	pruneNow   = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	detachedAt = pruneNow.Add(-48 * time.Hour)
)

func newFixture() *fixture {
	return &fixture{
		providers:  &memTable[provider.Provider]{metaOf: func(x *provider.Provider) *meta.Metadata { return &x.Meta }},
		hosts:      &memTable[host.Host]{metaOf: func(x *host.Host) *meta.Metadata { return &x.Meta }},
		models:     &memTable[model.Model]{metaOf: func(x *model.Model) *meta.Metadata { return &x.Meta }},
		pricings:   &memTable[pricing.Pricing]{metaOf: func(x *pricing.Pricing) *meta.Metadata { return &x.Meta }},
		bindings:   &memTable[binding.Binding]{metaOf: func(x *binding.Binding) *meta.Metadata { return &x.Meta }},
		policies:   &memTable[policy.Policy]{metaOf: func(x *policy.Policy) *meta.Metadata { return &x.Meta }},
		rateLimits: &memTable[ratelimit.RateLimit]{metaOf: func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta }},
		usage:      &usageLog{},
	}
}

func rowMeta(name string, detached bool) meta.Metadata {
	m := meta.Metadata{ID: "id-" + name, Name: name, ResourceVersion: "1", Owner: meta.Owner{Kind: meta.OwnerSystem}}
	if detached {
		at := detachedAt
		m.CatalogDetachedAt = &at
	}
	return m
}

func (f *fixture) stores() Stores {
	return Stores{
		Providers: f.providers, Hosts: f.hosts, Models: f.models, Pricings: f.pricings,
		Bindings: f.bindings, Policies: f.policies, RateLimits: f.rateLimits, Overlays: f.overlays,
	}
}

// references answers what the control API's scans would for these kinds:
// policies linking a rate limit, bindings and pricings naming a model.
func (f *fixture) references(_ context.Context, kind, id string) ([]Ref, error) {
	var out []Ref
	switch kind {
	case "rate-limit":
		for _, p := range f.policies.rows {
			if slices.ContainsFunc(p.Spec.RateLimits, func(l policy.RateLimitLink) bool { return l.RateLimitID == id }) {
				out = append(out, Ref{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name})
			}
		}
	case "model":
		for _, b := range f.bindings.rows {
			if b.Spec.ModelID == id {
				out = append(out, Ref{Kind: "host-binding", ID: b.Meta.ID, Name: b.Meta.Name})
			}
		}
		for _, p := range f.pricings.rows {
			if slices.Contains(p.Spec.TargetModelIDs, id) {
				out = append(out, Ref{Kind: "pricing", ID: p.Meta.ID, Name: p.Meta.Name})
			}
		}
	}
	return out, nil
}

func (f *fixture) run(t *testing.T, apply bool) Report {
	t.Helper()
	rep, err := Run(context.Background(), f.stores(), Options{
		Idle: 30 * 24 * time.Hour, Apply: apply, References: f.references, Usage: f.usage,
		Now: func() time.Time { return pruneNow },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func outcomes(rep Report) map[string]outcome {
	got := map[string]outcome{}
	for _, d := range rep.Rows {
		got[d.Kind+"/"+d.Name] = outcome{d.Decision, d.Reason}
	}
	return got
}

// A release dropped two rate limits: the one an operator's policy links stays
// detached, the other goes. A second run finds nothing more to delete.
func TestRunPrunesOnlyWhatNothingHolds(t *testing.T) {
	f := newFixture()
	a, b := &ratelimit.RateLimit{Meta: rowMeta("tier-a-rl", true)}, &ratelimit.RateLimit{Meta: rowMeta("tier-b-rl", true)}
	shipped := &ratelimit.RateLimit{Meta: rowMeta("tier-c-rl", false)}
	f.rateLimits.rows = []*ratelimit.RateLimit{a, b, shipped}
	f.policies.rows = []*policy.Policy{{Meta: meta.Metadata{ID: "id-team-default", Name: "team-default", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t1"}},
		Spec: policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: a.Meta.ID}}}}}

	want := map[string]outcome{
		"rate-limit/tier-a-rl": {Keep, "referenced by policy team-default"},
		"rate-limit/tier-b-rl": {Prune, ""},
	}
	dry := f.run(t, false)
	if diff := cmp.Diff(want, outcomes(dry)); diff != "" || dry.Applied || dry.Pruned != 1 || dry.Kept != 1 {
		t.Fatalf("dry run %+v (-want +got):\n%s", dry, diff)
	}
	if len(f.rateLimits.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", f.rateLimits.deleted)
	}

	rep := f.run(t, true)
	if diff := cmp.Diff(want, outcomes(rep)); diff != "" || !rep.Applied || rep.Pruned != 1 || rep.Kept != 1 {
		t.Fatalf("apply %+v (-want +got):\n%s", rep, diff)
	}
	if diff := cmp.Diff([]string{"tier-b-rl"}, f.rateLimits.deleted); diff != "" {
		t.Fatalf("deleted (-want +got):\n%s", diff)
	}

	again := f.run(t, true)
	if diff := cmp.Diff(map[string]outcome{"rate-limit/tier-a-rl": {Keep, "referenced by policy team-default"}}, outcomes(again)); diff != "" || again.Pruned != 0 {
		t.Fatalf("second run %+v (-want +got):\n%s", again, diff)
	}
	if len(f.rateLimits.deleted) != 1 {
		t.Fatalf("the second run deleted more: %v", f.rateLimits.deleted)
	}
}

// A dropped model goes together with its idle binding and the pricing only that
// binding used, bindings first so nothing is left pointing at a deleted row; a
// model with recent traffic, and its binding, stay.
func TestRunDeletesChildrenBeforeTheirParent(t *testing.T) {
	f := newFixture()
	idle, busy := &model.Model{Meta: rowMeta("old-chat", true)}, &model.Model{Meta: rowMeta("busy-chat", true)}
	f.models.rows = []*model.Model{idle, busy}
	f.bindings.rows = []*binding.Binding{
		{Meta: rowMeta("old-chat-on-acme", true), Spec: binding.Spec{ModelID: idle.Meta.ID, HostID: "id-acme"}},
		{Meta: rowMeta("busy-chat-on-acme", true), Spec: binding.Spec{ModelID: busy.Meta.ID, HostID: "id-acme"}},
	}
	f.pricings.rows = []*pricing.Pricing{{Meta: rowMeta("acme-old-chat", true), Spec: pricing.Spec{TargetModelIDs: []string{idle.Meta.ID}}}}
	f.usage.events = []usage.Event{
		{ModelID: idle.Meta.ID, HostID: "id-acme", Timestamp: pruneNow.Add(-40 * 24 * time.Hour)},
		{ModelID: busy.Meta.ID, HostID: "id-acme", Timestamp: pruneNow.Add(-2 * time.Hour)},
	}

	rep := f.run(t, true)
	traffic := "traffic since 2026-10-16T12:00:00Z (last request 2026-11-15T10:00:00Z)"
	want := map[string]outcome{
		"host-binding/old-chat-on-acme":  {Prune, ""},
		"host-binding/busy-chat-on-acme": {Keep, traffic},
		"pricing/acme-old-chat":          {Prune, ""},
		"model/old-chat":                 {Prune, ""},
		"model/busy-chat":                {Keep, traffic},
	}
	if diff := cmp.Diff(want, outcomes(rep)); diff != "" {
		t.Fatalf("decisions (-want +got):\n%s", diff)
	}
	if len(f.bindings.deleted) != 1 || len(f.pricings.deleted) != 1 || len(f.models.deleted) != 1 {
		t.Fatalf("deleted bindings %v, pricings %v, models %v", f.bindings.deleted, f.pricings.deleted, f.models.deleted)
	}
}

// A model an operator's policy names exactly stays; a provider-wide grant, a
// clean catalog policy, or a ref to another provider's model of the same name
// doesn't hold it.
func TestRunKeepsModelsAnOperatorPolicyNames(t *testing.T) {
	f := newFixture()
	acme := &provider.Provider{Meta: rowMeta("acme", false)}
	f.providers.rows = []*provider.Provider{acme}
	byAcme := meta.Owner{Kind: meta.OwnerProvider, ID: acme.Meta.ID}
	for _, n := range []string{"m1", "m2", "m3", "m4", "m5"} {
		m := &model.Model{Meta: rowMeta(n, true)}
		m.Meta.Owner = byAcme
		f.models.rows = append(f.models.rows, m)
	}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: "t1"}
	edited := rowMeta("acme-tier-edited", false)
	edited.Owner, edited.Dirty = meta.Owner{Kind: meta.OwnerHost, ID: "id-acme-host"}, true
	clean := rowMeta("acme-tier-clean", false)
	clean.Owner = meta.Owner{Kind: meta.OwnerHost, ID: "id-acme-host"}
	f.policies.rows = []*policy.Policy{
		{Meta: meta.Metadata{ID: "id-team-default", Name: "team-default", Owner: team}, Spec: policy.Spec{Models: []string{"acme/m1", "acme/m1@acme-host", "acme"}}},
		{Meta: edited, Spec: policy.Spec{Models: []string{"acme/m4@acme-host"}}},
		{Meta: clean, Spec: policy.Spec{Models: []string{"acme/m3"}}},
		{Meta: meta.Metadata{ID: "id-other", Name: "other-grant", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p1"}}, Spec: policy.Spec{Models: []string{"other/m5"}}},
	}
	rep := f.run(t, true)
	want := map[string]outcome{
		"model/m1": {Keep, "named by policy team-default"},
		"model/m2": {Prune, ""},
		"model/m3": {Prune, ""},
		"model/m4": {Keep, "named by policy acme-tier-edited"},
		"model/m5": {Prune, ""},
	}
	if diff := cmp.Diff(want, outcomes(rep)); diff != "" {
		t.Fatalf("decisions (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"m2", "m3", "m5"}, f.models.deleted); diff != "" {
		t.Fatalf("deleted (-want +got):\n%s", diff)
	}
}

// A row written between the decision and its delete stays, and so does a row
// something started referencing meanwhile.
func TestRunKeepsRowsThatChangeDuringTheRun(t *testing.T) {
	f := newFixture()
	f.rateLimits.rows = []*ratelimit.RateLimit{{Meta: rowMeta("edited-rl", true)}, {Meta: rowMeta("linked-rl", true)}}
	calls := map[string]int{}
	refs := func(ctx context.Context, kind, id string) ([]Ref, error) {
		calls[id]++
		// The second read of each row is the one its delete makes.
		if calls[id] == 2 {
			switch id {
			case "id-edited-rl":
				f.rateLimits.edit("edited-rl")
			case "id-linked-rl":
				return []Ref{{Kind: "policy", ID: "id-new", Name: "new-policy"}}, nil
			}
		}
		return f.references(ctx, kind, id)
	}
	rep, err := Run(context.Background(), f.stores(), Options{Idle: time.Hour, Apply: true, References: refs, Usage: f.usage})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]outcome{
		"rate-limit/edited-rl": {Keep, "changed during the prune"},
		"rate-limit/linked-rl": {Keep, "referenced by policy new-policy"},
	}
	if diff := cmp.Diff(want, outcomes(rep)); diff != "" || rep.Pruned != 0 || len(f.rateLimits.deleted) != 0 {
		t.Fatalf("report %+v, deleted %v (-want +got):\n%s", rep, f.rateLimits.deleted, diff)
	}
}

// Without an answer from the usage store a model or binding stays; one failed
// read is enough to stop asking. Kinds without the guard are still judged, and
// an overlay keeps its model whatever else holds.
func TestRunKeepsWhatUsageCannotAnswerFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  UsageReader
		reason string
	}{
		{"no usage store", nil, "usage unknown: no usage store is configured"},
		{"usage store failing", &usageLog{err: errors.New("clickhouse: connection refused")}, "usage unknown: clickhouse: connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.models.rows = []*model.Model{{Meta: rowMeta("m1", true)}, {Meta: rowMeta("m2", true)}, {Meta: rowMeta("m3", true)}}
			f.overlays = memOverlays{{Kind: overlay.KindModel, ResourceID: "id-m3"}}
			f.rateLimits.rows = []*ratelimit.RateLimit{{Meta: rowMeta("rl", true)}}
			rep, err := Run(context.Background(), f.stores(), Options{Idle: time.Hour, Apply: true, References: f.references, Usage: tc.usage})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]outcome{
				"model/m1": {Keep, tc.reason}, "model/m2": {Keep, tc.reason}, "model/m3": {Keep, "overlay"},
				"rate-limit/rl": {Prune, ""},
			}
			if diff := cmp.Diff(want, outcomes(rep)); diff != "" {
				t.Fatalf("decisions (-want +got):\n%s", diff)
			}
			if u, ok := tc.usage.(*usageLog); ok && u.reads != 1 {
				t.Fatalf("usage reads = %d, want 1", u.reads)
			}
		})
	}
}

// A row with the detach mark that is owned by the organization is an operator's, never a prune candidate, even when the mark outlived the move.
func TestRunLeavesOrganizationRowsAlone(t *testing.T) {
	f := newFixture()
	org := &policy.Policy{Meta: rowMeta("moved-policy", true)}
	org.Meta.Owner = meta.Owner{Kind: meta.OwnerOrganization}
	orgRL := &ratelimit.RateLimit{Meta: rowMeta("moved-rl", true)}
	orgRL.Meta.Owner = meta.Owner{Kind: meta.OwnerOrganization}
	f.policies.rows = []*policy.Policy{org}
	f.rateLimits.rows = []*ratelimit.RateLimit{orgRL}
	rep := f.run(t, true)
	if len(outcomes(rep)) != 0 || rep.Pruned != 0 || len(f.policies.deleted)+len(f.rateLimits.deleted) != 0 {
		t.Fatalf("prune judged organization rows: %+v, deleted %v %v", outcomes(rep), f.policies.deleted, f.rateLimits.deleted)
	}
}
