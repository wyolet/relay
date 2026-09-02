package authz_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/pkg/ids"
)

// builtinRole returns the named built-in rule set.
func builtinRole(t *testing.T, name string) *role.Role {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	for _, r := range builtins {
		if r.Meta.Name == name {
			r.Meta.ID = ids.New()
			return r
		}
	}
	t.Fatalf("no built-in role %q", name)
	return nil
}

// A team admin may bind their own role at their own team, but not a role
// granting more than they hold — binding `admin` at team scope would hand
// themselves every verb in the deployment.
func TestCheckGrantRefusesEscalation(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	alice := ctxOf(actorOf(aliceID, subjectsOf(aliceID))) // team-admin on T1
	scope := meta.Owner{Kind: meta.OwnerTeam, ID: t1ID}

	if err := authz.CheckGrant(alice, rbac, builtinRole(t, "team-admin"), scope); err != nil {
		t.Fatalf("binding their own role at their own team: %v", err)
	}
	err := authz.CheckGrant(alice, rbac, builtinRole(t, "admin"), scope)
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("binding admin at team scope = %v, want forbidden", err)
	}
	// Their own role at a team they do not administer is refused too.
	if err := authz.CheckGrant(alice, rbac, builtinRole(t, "team-admin"),
		meta.Owner{Kind: meta.OwnerTeam, ID: t2ID}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("binding at a foreign team = %v, want forbidden", err)
	}
	// An admin binds anything, and so does a loader with no authorizer.
	admin := ctxOf(&actor.Actor{AdminToken: true})
	if err := authz.CheckGrant(admin, rbac, builtinRole(t, "admin"), scope); err != nil {
		t.Fatalf("admin binding admin: %v", err)
	}
	if err := authz.CheckGrant(alice, nil, builtinRole(t, "admin"), scope); err != nil {
		t.Fatalf("nil authorizer: %v", err)
	}
}
