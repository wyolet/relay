package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
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

// allowAll grants every action and scopes nothing.
type allowAll struct{}

func (allowAll) Authorize(context.Context, string, authz.Resource) error { return nil }

func personalAccountGroupRows() (*Rows, *group.Group) {
	alice := &user.User{ID: meta.NewID(), Username: "alice@example.test"}
	bob := &user.User{ID: meta.NewID(), Username: "bob@example.test"}
	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "probe", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{alice.ID}, PersonalAccount: "off"}}
	return &Rows{Users: []*user.User{alice, bob}, Groups: []*group.Group{g}}, g
}

// A group that turns personal accounts on or off is an admin's through apply too: a document changing its members is refused for anyone else, and an admin's keeps the stored value the document cannot carry.
func TestApplyGroupWithPersonalAccountNeedsAnAdmin(t *testing.T) {
	rows, _ := personalAccountGroupRows()
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
	if err := b.run(context.Background(), []manifest.Document{groupDoc("bob@example.test")}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("non-admin changing members = %v, want forbidden", err)
	}
	b = &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), operator: true}
	if err := b.run(context.Background(), []manifest.Document{groupDoc("bob@example.test")}); err != nil {
		t.Fatalf("admin changing members = %v", err)
	}
	if e := entryFor(t, b, "Group", "probe"); e.Action != ActionUpdate {
		t.Fatalf("admin's plan = %s, want update", e.Action)
	}
}

// Pruning such a group is an admin's as well.
func TestApplyPruneGroupWithPersonalAccountNeedsAnAdmin(t *testing.T) {
	rows, g := personalAccountGroupRows()
	g.Meta.Labels = pruneLabels
	b := pruneAll(rows)
	if err := b.run(context.Background(), nil); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("non-admin prune = %v, want forbidden", err)
	}
	b = pruneAll(rows)
	b.operator = true
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("admin prune = %v", err)
	}
}

// Apply refuses what the control API refuses while a user's personal account is off, with the same code: a provider account they would own, and a share with them.
func TestApplyRefusesPersonalThingsWhilePersonalAccountIsOff(t *testing.T) {
	bob := &user.User{ID: meta.NewID(), Username: "bob"}
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	src := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: h.Meta.ID}}
	rows := &Rows{Users: []*user.User{bob}, Hosts: []*host.Host{h}, Accounts: []*account.Account{src}}
	off := func(_ context.Context, id string) (bool, string) { return id == bob.ID, "bob" }
	for name, doc := range map[string]string{
		"provider account": "kind: Account\nmetadata: {name: bobs-org, owner: {kind: user, name: bob}}\nspec: {host: acme-api}\n",
		"share":            "kind: Account\nmetadata: {name: acme-for-bob, owner: {kind: user, name: bob}}\nspec: {host: acme-api, sourceAccount: acme-org}\n",
	} {
		b := &builder{opts: Options{Stores: &Stores{}, Authz: allowAll{}, PersonalAccountOff: off}, rows: rows, idx: newIndex(rows), admin: true, operator: true}
		err := b.run(context.Background(), userDocs(t, docHeader+doc))
		var re *refcheck.Error
		if !errors.As(err, &re) || re.Code != refcheck.CodePersonalAccountOff {
			t.Errorf("%s: %v, want %s", name, err, refcheck.CodePersonalAccountOff)
		}
	}
}
