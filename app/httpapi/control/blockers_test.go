package control

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/meta"
)

func TestGroupBlockers(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	mine := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	elsewhere := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
	// A list field whose last entry is the target cannot let go, so rb-1 and rb-2 share a field but not a group.
	items := func() []referenceItem {
		return []referenceItem{
			{Kind: "key", ID: "k-2", Name: "zeta", Via: "spec.policyId", owner: elsewhere, detachable: true},
			{Kind: "role-binding", ID: "rb-1", Name: "sole", Via: "spec.subjects", owner: system},
			{Kind: "host", ID: "h-1", Name: "host-a", Via: "spec.policies", owner: system, detachable: true},
			{Kind: "key", ID: "k-1", Name: "alpha", Via: "spec.policyId", owner: mine, detachable: true},
			{Kind: "policy-binding", ID: "pb-1", Name: "gone", Via: "spec.accountIds", owner: elsewhere, deleted: true, shownToSharer: true},
			{Kind: "role-binding", ID: "rb-2", Name: "shared", Via: "spec.subjects", owner: system, detachable: true},
			{Kind: "key", ID: "k-3", Name: "beta", Via: "spec.accountId", owner: mine},
			{Kind: "host-key", ID: "hk-1", Name: "tier", Via: "spec.policyId", owner: elsewhere},
		}
	}
	item := func(id, name string) []blockerItem { return []blockerItem{{ID: id, Name: name}} }

	// A row in a scope the caller cannot see still counts, unnamed, unless it is a share's row shown to the sharer.
	for _, tc := range []struct {
		who  string
		want []blockerGroup
	}{
		{"alice", []blockerGroup{
			{Kind: "host", Field: "spec.policies", Detachable: true, Count: 1, Items: item("h-1", "host-a")},
			{Kind: "host-key", Field: "spec.policyId", Count: 1, Hidden: 1, Items: []blockerItem{}},
			{Kind: "key", Field: "spec.accountId", Count: 1, Items: item("k-3", "beta")},
			{Kind: "key", Field: "spec.policyId", Detachable: true, Count: 2, Hidden: 1, Items: item("k-1", "alpha")},
			{Kind: "policy-binding", Field: "spec.accountIds", Deleted: true, Count: 1, Items: item("pb-1", "gone")},
			{Kind: "role-binding", Field: "spec.subjects", Detachable: true, Count: 1, Hidden: 1, Items: []blockerItem{}},
			{Kind: "role-binding", Field: "spec.subjects", Count: 1, Hidden: 1, Items: []blockerItem{}},
		}},
		{"root", []blockerGroup{
			{Kind: "host", Field: "spec.policies", Detachable: true, Count: 1, Items: item("h-1", "host-a")},
			{Kind: "host-key", Field: "spec.policyId", Count: 1, Items: item("hk-1", "tier")},
			{Kind: "key", Field: "spec.accountId", Count: 1, Items: item("k-3", "beta")},
			{Kind: "key", Field: "spec.policyId", Detachable: true, Count: 2, Items: []blockerItem{{ID: "k-1", Name: "alpha"}, {ID: "k-2", Name: "zeta"}}},
			{Kind: "policy-binding", Field: "spec.accountIds", Deleted: true, Count: 1, Items: item("pb-1", "gone")},
			{Kind: "role-binding", Field: "spec.subjects", Detachable: true, Count: 1, Items: item("rb-2", "shared")},
			{Kind: "role-binding", Field: "spec.subjects", Count: 1, Items: item("rb-1", "sole")},
		}},
	} {
		got := groupBlockers(actor.WithActor(t.Context(), scopeActors[tc.who]), testRBAC(), items())
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.who, diff)
		}
	}
	if got := groupBlockers(t.Context(), testRBAC(), nil); got == nil || len(got) != 0 {
		t.Errorf("no references = %#v, want an empty list", got)
	}
}
