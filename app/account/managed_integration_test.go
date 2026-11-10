//go:build integration

package account_test

import (
	"context"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// Concurrent first saves of one caller account converge on one row; a second place whose name is taken gets a suffix; the membership marks round-trip; deleting the user or the project takes the accounts along.
func TestIntegration_EnsureManaged(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "search", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	alice := &user.User{ID: meta.NewID(), Username: "alice"}
	users := user.NewStore(db)
	if err := users.Upsert(ctx, alice); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	store := account.NewStore(gen.New(db))
	scope := meta.Owner{Kind: meta.OwnerProject, ID: prj.Meta.ID}

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i], errs[i] = store.EnsureManaged(ctx, account.Member(alice.ID, alice.Username, scope, prj.Meta.Name, ""))
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("save %d = %q, %v; want %q", i, ids[i], errs[i], ids[0])
		}
	}
	member, err := store.Get(ctx, ids[0])
	if err != nil || member == nil || member.DerivedKind() != account.KindMember || member.Meta.Owner != scope || member.Spec.Holder == nil || member.Spec.Holder.ID != alice.ID {
		t.Fatalf("member account = %+v (err %v), want alice's member account in the project", member, err)
	}

	// The project's own account is named project-search; a second place wanting the same name gets a suffix.
	own, err := store.EnsureManaged(ctx, account.ForProject(prj.Meta.ID, prj.Meta.Name, ""))
	if err != nil {
		t.Fatalf("project account: %v", err)
	}
	clash := account.Personal(alice.ID, alice.Username)
	clash.Meta.Name = "project-search"
	personal, err := store.EnsureManaged(ctx, clash)
	if err != nil {
		t.Fatalf("personal account: %v", err)
	}
	if got, _ := store.Get(ctx, personal); got == nil || got.Meta.Name != "project-search-2" {
		t.Fatalf("the clashing account = %+v, want the name suffixed", got)
	}

	if err := store.SetMembership(ctx, member, true, true); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	closed, _ := store.Get(ctx, member.Meta.ID)
	if closed == nil || !closed.Spec.Closed || !closed.FollowsRoleBindings {
		t.Fatalf("after SetMembership = %+v, want closed and following role bindings", closed)
	}
	// A close decided from an older read (here the reopen below wins) changes nothing.
	if err := store.SetMembership(ctx, closed, false, true); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.SetMembership(ctx, closed, true, true); err != nil {
		t.Fatalf("stale close: %v", err)
	}
	if got, _ := store.Get(ctx, member.Meta.ID); got == nil || got.Spec.Closed {
		t.Fatalf("after a stale close = %+v, want the reopened account left open", got)
	}

	if err := users.Delete(ctx, alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	for _, id := range []string{member.Meta.ID, personal} {
		if got, err := store.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("account %s after its holder was deleted = %+v (err %v), want gone", id, got, err)
		}
	}
	if err := project.NewStore(db).Delete(ctx, prj.Meta.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if got, err := store.Get(ctx, own); err != nil || got != nil {
		t.Fatalf("the project's account after the project was deleted = %+v (err %v), want gone", got, err)
	}
}
