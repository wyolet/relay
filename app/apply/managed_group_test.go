package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

func managedGroupRows() *Rows {
	return &Rows{Groups: []*group.Group{{
		Meta:     meta.Metadata{ID: meta.NewID(), Name: "engineering", Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: pruneLabels},
		External: &group.External{Source: "okta", ID: "00g1"},
	}}}
}

// A group a provisioning source manages is refused while planning, so a bundle that also creates a team writes neither; force writes over it.
func TestPlanRefusesAManagedGroupBeforeWriting(t *testing.T) {
	docs := userDocs(t, "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: core, owner: {kind: system}}\nspec: {}\n"+
		"---\napiVersion: relay.wyolet.dev/v1alpha2\nkind: Group\nmetadata: {name: engineering, displayName: Engineering, owner: {kind: system}}\nspec: {}\n")
	rows := managedGroupRows()
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), admin: true}
	err := b.run(context.Background(), docs)
	me, ok := errors.AsType[*directory.ManagedError](err)
	if !ok || me.Name != "engineering" || me.Source != "okta" {
		t.Fatalf("err = %v, want the managed group refused while planning", err)
	}

	rows = managedGroupRows()
	b = &builder{opts: Options{Stores: &Stores{}, Force: true}, rows: rows, idx: newIndex(rows), admin: true}
	if err := b.run(context.Background(), docs); err != nil {
		t.Fatalf("a forced plan: %v", err)
	}
}

// Pruning a managed group is refused while planning too, unless forced.
func TestPruneRefusesAManagedGroupBeforeWriting(t *testing.T) {
	b := pruneAll(managedGroupRows())
	if _, ok := errors.AsType[*directory.ManagedError](b.run(context.Background(), nil)); !ok {
		t.Fatal("the prune of a managed group was planned")
	}
	b = pruneAll(managedGroupRows())
	b.opts.Force = true
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("a forced prune: %v", err)
	}
	for _, e := range b.entries {
		if e.Kind == "Group" && e.Name == "engineering" && e.Action == ActionDelete {
			return
		}
	}
	t.Fatalf("a forced prune did not delete the group: %+v", b.entries)
}
