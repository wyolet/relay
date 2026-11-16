package apply

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// The catalog seed owns the system's rows only: a stored row of a catalog document's name that the organization or a tenant owns is left as it is, owner and content, and counted as a conflict.
func TestCatalogSeedLeavesOperatorRowsOfACatalogName(t *testing.T) {
	storedAs := func(owner meta.Owner) *ratelimit.RateLimit {
		r := dirtyRateLimit()
		r.Meta.Dirty, r.Meta.Owner = false, owner
		return r
	}
	doc := rateLimitDoc("burst")
	doc.Spec.Rules[0].Amount = 99
	for _, owner := range []meta.Owner{{Kind: meta.OwnerOrganization}, {Kind: meta.OwnerTeam, ID: "t-1"}, {Kind: meta.OwnerUser, ID: "u-alice"}} {
		t.Run(string(owner.Kind), func(t *testing.T) {
			stored := storedAs(owner)
			before := *stored
			b := &builder{opts: Options{CatalogSeed: true, Force: true}, rows: &Rows{RateLimits: []*ratelimit.RateLimit{stored}}, lic: license.Community}
			entries, written, err := rateLimitPlan(t, b, doc)
			if err != nil {
				t.Fatalf("planKind: %v", err)
			}
			if len(entries) != 1 || entries[0].Action != ActionConflict || entries[0].ID != stored.Meta.ID {
				t.Fatalf("entries = %+v, want one conflict on the stored row", entries)
			}
			if len(written) != 0 || !cmp.Equal(before, *stored) {
				t.Fatalf("the seed wrote %+v or changed the stored row (-before +after):\n%s", written, cmp.Diff(before, *stored))
			}
		})
	}
	// Outside the catalog seed the same document is an ordinary update.
	b := &builder{rows: &Rows{RateLimits: []*ratelimit.RateLimit{storedAs(meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})}}, lic: license.Community}
	entries, _, err := rateLimitPlan(t, b, doc)
	if err != nil || len(entries) != 1 || entries[0].Action != ActionUpdate {
		t.Fatalf("apply = %+v, %v; want an update", entries, err)
	}
}
