package manifest_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

const managedGroups = "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Group\nmetadata: {name: ops, displayName: Ops, labels: {owner: gitops}}\n"

// A group a provisioning source manages is left alone by apply and prune unless the run is forced, and a forced write keeps the source.
func TestPlanSkipsManagedGroupsUnlessForced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ext := directory.External{Source: "scim-1", ID: "g-1"}
	g := &directory.Group{Meta: directory.Metadata{Name: "ops", Labels: map[string]string{"owner": "gitops"}}, Spec: directory.GroupSpec{External: ext}}
	if err := e.dir.WithWriter(directory.Writer{Source: "scim-1"}).CreateGroup(ctx, admin, g); err != nil {
		t.Fatal(err)
	}

	wantLines(t, actions(e.apply(t, managedGroups, manifest.Options{})), []string{"skip-managed Group/ops metadata.displayName"})
	prune := manifest.Options{Prune: true, Selector: map[string]string{"owner": "gitops"}}
	wantLines(t, actions(e.apply(t, "", prune)), []string{"skip-managed Group/ops"})

	wantLines(t, actions(e.apply(t, managedGroups, manifest.Options{Force: true})), []string{"update Group/ops metadata.displayName"})
	got, _ := e.store.Stores().Groups.Get(ctx, g.Meta.ID)
	if got.Meta.DisplayName != "Ops" || got.Spec.External != ext {
		t.Fatalf("forced apply left %+v", got)
	}
	prune.Force = true
	wantLines(t, actions(e.apply(t, "", prune)), []string{"delete Group/ops"})
	if got, _ := e.store.Stores().Groups.Get(ctx, g.Meta.ID); got != nil {
		t.Fatalf("forced prune left %+v", got)
	}
}
