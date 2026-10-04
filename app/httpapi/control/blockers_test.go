package control

import (
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
)

func TestGroupBlockers_GroupsByKindAndFieldAndHidesUnseenRows(t *testing.T) {
	ctx := actor.WithActor(t.Context(), scopeActors["alice"])
	mine := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	elsewhere := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
	items := []referenceItem{
		{Kind: "key", ID: "k-2", Name: "zeta", Via: "spec.policyId", owner: elsewhere, detachable: true},
		{Kind: "host", ID: "h-1", Name: "host-a", Via: "spec.policies", owner: meta.Owner{Kind: meta.OwnerSystem}, detachable: true},
		{Kind: "key", ID: "k-1", Name: "alpha", Via: "spec.policyId", owner: mine, detachable: true},
		{Kind: "host-key", ID: "hk-1", Name: "tier", Via: "spec.policyId", owner: elsewhere},
	}

	groups := groupBlockers(ctx, testRBAC(), items)
	if len(groups) != 3 {
		t.Fatalf("groups = %+v, want host, host-key, key", groups)
	}
	host, hostKey, keys := groups[0], groups[1], groups[2]
	if host.Kind != "host" || !host.Detachable || host.Count != 1 || len(host.Items) != 1 {
		t.Errorf("host group = %+v", host)
	}
	if hostKey.Kind != "host-key" || hostKey.Detachable || hostKey.Count != 1 || hostKey.Hidden != 1 || len(hostKey.Items) != 0 {
		t.Errorf("host-key group = %+v, want one hidden, not detachable", hostKey)
	}
	if keys.Kind != "key" || keys.Count != 2 || keys.Hidden != 1 || len(keys.Items) != 1 || keys.Items[0].ID != "k-1" {
		t.Errorf("key group = %+v, want alice's key listed and one hidden", keys)
	}
}
