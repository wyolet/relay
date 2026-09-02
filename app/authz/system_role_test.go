package authz

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
)

// Creating a role binding means reading the Role it names. The built-ins are
// system-owned and live at the global scope, where a team or project admin
// holds no binding — so without a read shortcut they can never bind anyone.
func TestSystemOwnedRolesAreReadable(t *testing.T) {
	r := RBAC{} // no snapshot: only the shortcut can admit the call
	ctx := actor.WithActor(context.Background(), &actor.Actor{
		UserID: "u-1", Username: "alice", Subjects: []string{"user:u-1"},
	})
	system := meta.Owner{Kind: meta.OwnerSystem}

	for _, action := range []string{"roles.get", "roles.list"} {
		if err := r.Authorize(ctx, action, Resource{Kind: "role", ID: "r-1", Owner: &system}); err != nil {
			t.Errorf("%s on a system role = %v, want allowed", action, err)
		}
	}
	// Reading is not editing.
	if err := r.Authorize(ctx, "roles.update", Resource{Kind: "role", ID: "r-1", Owner: &system}); err == nil {
		t.Error("roles.update on a system role must still need a binding")
	}
	// A tenant-scoped role is not covered by the shortcut.
	team := meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	if err := r.Authorize(ctx, "roles.get", Resource{Kind: "role", ID: "r-2", Owner: &team}); err == nil {
		t.Error("roles.get on a team-owned role must need a binding")
	}
}
