package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// An export from before the organization owner names the moved rows system. Applied again by an admin, whose document owner wins, it keeps them in the organization; a built-in rate limit, stored system-owned, stays system.
func TestApplySystemOwnerFromBefore(t *testing.T) {
	system := manifest.WireOwner{Kind: meta.OwnerSystem}
	for _, tc := range []struct {
		name   string
		stored meta.Owner
		want   meta.Owner
	}{
		{"an organization rate limit", meta.Global, meta.Global},
		{"a built-in rate limit", meta.Owner{Kind: meta.OwnerSystem}, meta.Owner{Kind: meta.OwnerSystem}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := dirtyRateLimit()
			stored.Meta.Dirty, stored.Meta.Owner = false, tc.stored
			doc := rateLimitDoc("burst")
			doc.Metadata.Owner = system
			b := &builder{admin: true, rows: &Rows{RateLimits: []*ratelimit.RateLimit{stored}}, lic: license.Community}
			entries, _, err := rateLimitPlan(t, b, doc)
			if err != nil || len(entries) != 1 || entries[0].owner != tc.want {
				t.Fatalf("plan = %+v, %v; want the row kept owned by %+v", entries, err, tc.want)
			}
		})
	}

	// A kind with no built-in rows: system always names the organization, for a new row too.
	b := &builder{admin: true, rows: &Rows{Budgets: []*budget.Budget{{Meta: meta.Metadata{ID: meta.NewID(), Name: "month", Owner: meta.Global}, Spec: budget.Spec{AmountUSD: "10"}}}}, lic: license.Community}
	docs := []*manifest.BudgetDTO{
		{APIVersion: manifest.APIVersion, Kind: "Budget", Metadata: manifest.WireMeta{Name: "month", Owner: system}, Spec: manifest.BudgetSpec{AmountUSD: "10"}},
		{APIVersion: manifest.APIVersion, Kind: "Budget", Metadata: manifest.WireMeta{Name: "new", Owner: system}, Spec: manifest.BudgetSpec{AmountUSD: "5"}},
	}
	written, err := planOne(context.Background(), b, kindWiring[manifest.BudgetDTO, budget.Budget]{
		Kind: "Budget", Docs: docs, Names: map[string]string{"month": b.rows.Budgets[0].Meta.ID}, Rows: b.rows.Budgets,
		To: manifest.ToBudget, Meta: func(x *budget.Budget) *meta.Metadata { return &x.Meta },
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	runWrites(t, b)
	if len(b.entries) != 2 || len(*written) != 2 {
		t.Fatalf("entries %+v, %d writes; want an update and a create", b.entries, len(*written))
	}
	for _, e := range b.entries {
		if e.owner != meta.Global {
			t.Errorf("%s %s owner = %+v, want the organization", e.Kind, e.Name, e.owner)
		}
	}
	for _, x := range *written {
		if x.Meta.Owner != meta.Global {
			t.Errorf("wrote %s owned by %+v, want the organization", x.Meta.Name, x.Meta.Owner)
		}
	}
}
