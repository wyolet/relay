package seed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

func tierPolicy(name string, owner meta.Owner) *policy.Policy {
	return &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}}
}

// markPolicies runs one markRows pass over rows with declared as the tree's
// Policy names, recording what would be written.
func markPolicies(t *testing.T, o origin, declared []string, rows ...*policy.Policy) (*Result, []string) {
	t.Helper()
	names := map[string]bool{}
	for _, n := range declared {
		names[n] = true
	}
	res := &Result{}
	var written []string
	mk := marker{declared: map[string]map[string]bool{"Policy": names}, origin: o, now: time.Now(), res: res}
	err := markRows(context.Background(), mk, "Policy", rows,
		func(p *policy.Policy) *meta.Metadata { return &p.Meta },
		func(_ context.Context, p *policy.Policy) error {
			written = append(written, p.Meta.Name)
			return nil
		})
	if err != nil {
		t.Fatalf("markRows: %v", err)
	}
	return res, written
}

func TestMarkRowsDetachesOnlyDroppedCatalogRows(t *testing.T) {
	catalogHost, byoHost := meta.NewID(), meta.NewID()
	o := origin{hosts: map[string]meta.Owner{
		catalogHost: {Kind: meta.OwnerSystem},
		byoHost:     {}, // created through the API: no owner kind
	}}
	shipped := tierPolicy("acme-tier-1", meta.Owner{Kind: meta.OwnerHost, ID: catalogHost})
	dropped := tierPolicy("acme-tier-0", meta.Owner{Kind: meta.OwnerHost, ID: catalogHost})
	droppedDirty := tierPolicy("acme-tier-legacy", meta.Owner{Kind: meta.OwnerHost, ID: catalogHost})
	droppedDirty.Meta.Dirty = true
	byoTier := tierPolicy("byo-tier", meta.Owner{Kind: meta.OwnerHost, ID: byoHost})
	orphan := tierPolicy("orphan-tier", meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()})
	tenant := tierPolicy("team-policy", meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()})
	personal := tierPolicy("my-policy", meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()})

	res, written := markPolicies(t, o, []string{"acme-tier-1"},
		shipped, dropped, droppedDirty, byoTier, orphan, tenant, personal)

	if res.Detached != 2 || len(written) != 2 || written[0] != "acme-tier-0" || written[1] != "acme-tier-legacy" {
		t.Fatalf("detached %d, written %v; want the clean and the dirty dropped catalog tiers", res.Detached, written)
	}
	if dropped.Meta.CatalogDetachedAt == nil || droppedDirty.Meta.CatalogDetachedAt == nil {
		t.Fatal("a dropped catalog tier was not marked")
	}
	if !droppedDirty.Meta.Dirty {
		t.Fatal("marking cleared the dirty flag")
	}
	for _, p := range []*policy.Policy{shipped, byoTier, orphan, tenant, personal} {
		if p.Meta.CatalogDetachedAt != nil {
			t.Errorf("%s was marked", p.Meta.Name)
		}
	}

	// A second pass over the same tree changes nothing.
	res, written = markPolicies(t, o, []string{"acme-tier-1"},
		shipped, dropped, droppedDirty, byoTier, orphan, tenant, personal)
	if res.Detached != 0 || res.Reattached != 0 || len(written) != 0 {
		t.Fatalf("second pass: %+v, written %v; want no writes", res, written)
	}
}

func TestMarkRowsReattachesOnlyCleanReturningRows(t *testing.T) {
	host := meta.NewID()
	o := origin{hosts: map[string]meta.Owner{host: {Kind: meta.OwnerSystem}}}
	at := time.Now().Add(-time.Hour)
	clean := tierPolicy("acme-tier-1", meta.Owner{Kind: meta.OwnerHost, ID: host})
	clean.Meta.CatalogDetachedAt = &at
	dirty := tierPolicy("acme-tier-2", meta.Owner{Kind: meta.OwnerHost, ID: host})
	dirty.Meta.CatalogDetachedAt = &at
	dirty.Meta.Dirty = true

	res, written := markPolicies(t, o, []string{"acme-tier-1", "acme-tier-2"}, clean, dirty)
	if res.Reattached != 1 || len(written) != 1 || written[0] != "acme-tier-1" {
		t.Fatalf("reattached %d, written %v; want only the clean row", res.Reattached, written)
	}
	if clean.Meta.CatalogDetachedAt != nil {
		t.Fatal("the clean returning row is still detached")
	}
	if dirty.Meta.CatalogDetachedAt == nil {
		t.Fatal("the dirty returning row was re-attached")
	}
}

func TestOriginFromCatalog(t *testing.T) {
	provider, host := meta.NewID(), meta.NewID()
	o := origin{
		providers: map[string]meta.Owner{provider: {Kind: meta.OwnerSystem}},
		hosts:     map[string]meta.Owner{host: {Kind: meta.OwnerSystem}},
	}
	cases := []struct {
		kind  string
		owner meta.Owner
		want  bool
	}{
		{"Provider", meta.Owner{Kind: meta.OwnerSystem}, true},
		{"Host", meta.Owner{Kind: meta.OwnerSystem}, true},
		{"HostBinding", meta.Owner{Kind: meta.OwnerSystem}, true},
		{"Model", meta.Owner{Kind: meta.OwnerProvider, ID: provider}, true},
		{"Pricing", meta.Owner{Kind: meta.OwnerHost, ID: host}, true},
		{"RateLimit", meta.Owner{Kind: meta.OwnerHost, ID: host}, true},
		{"RateLimit", meta.Owner{Kind: meta.OwnerSystem}, false},
		{"Host", meta.Owner{}, false},
		{"Policy", meta.Owner{Kind: meta.OwnerProject, ID: "p1"}, false},
	}
	for _, c := range cases {
		m := &meta.Metadata{Owner: c.owner}
		if got := o.fromCatalog(c.kind, m); got != c.want {
			t.Errorf("%s owned by %+v: fromCatalog = %v, want %v", c.kind, c.owner, got, c.want)
		}
	}
}

// A failed write fails the pass, so the caller writes no catalog marker.
func TestMarkRowsReportsAWriteFailure(t *testing.T) {
	host := meta.NewID()
	o := origin{hosts: map[string]meta.Owner{host: {Kind: meta.OwnerSystem}}}
	mk := marker{declared: map[string]map[string]bool{}, origin: o, now: time.Now(), res: &Result{}}
	stale := errors.New("stale")
	err := markRows(context.Background(), mk, "Policy",
		[]*policy.Policy{tierPolicy("acme-tier-0", meta.Owner{Kind: meta.OwnerHost, ID: host})},
		func(p *policy.Policy) *meta.Metadata { return &p.Meta },
		func(context.Context, *policy.Policy) error { return stale })
	if !errors.Is(err, stale) {
		t.Fatalf("err = %v, want the write failure", err)
	}
}
