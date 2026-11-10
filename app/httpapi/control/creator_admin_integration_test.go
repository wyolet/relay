//go:build integration

package control

import (
	"context"
	"log/slog"
	"testing"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

// Granting the creator twice leaves one binding; the admin token gets none.
func TestGrantCreatorAdminIsIdempotent(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if err := role.SeedBuiltins(ctx, stores.Role, slog.Default(), nil); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	users := user.NewStore(pool)
	u := &user.User{ID: meta.NewID(), Username: "maker"}
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "made", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores, Users: users}
	ops := newDirectoryOps(d)
	scope := meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}
	count := func() int {
		all, err := stores.RoleBinding.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range all {
			if b.Spec.Scope == scope {
				n++
			}
		}
		return n
	}

	token := actor.WithActor(ctx, &actor.Actor{AdminToken: true})
	if err := grantCreatorAdmin(token, d, ops, scope, tm.Meta, role.BuiltinTeamAdmin); err != nil || count() != 0 {
		t.Fatalf("admin token: err %v, %d bindings; want none", err, count())
	}
	maker := actor.WithActor(context.Background(), &actor.Actor{UserID: u.ID, Username: u.Username})
	for range 2 {
		if err := grantCreatorAdmin(maker, d, ops, scope, tm.Meta, role.BuiltinTeamAdmin); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("%d bindings at the team after two grants, want 1", n)
	}
}
