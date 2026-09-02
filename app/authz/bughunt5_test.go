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

// A binding at a project grants get/list on the team that project belongs
// to: resolving the parent of the project you work in is part of the job.
func TestProjectBindingReadsTheParentTeam(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	frank := ctxOf(actorOf(frankID, subjectsOf(frankID, "ml"))) // project-admin on P2 (team T1)

	if err := rbac.Authorize(frank, "teams.get", authz.Resource{Kind: "team", ID: t1ID, Owner: &globalScope}); err != nil {
		t.Fatalf("project-bound actor reading the parent team = %v", err)
	}
	if !rbac.Visible(frank, "team", t1ID, globalScope) {
		t.Error("parent team is not visible to a project-bound actor")
	}
	if rbac.Visible(frank, "team", t2ID, globalScope) {
		t.Error("an unrelated team is visible")
	}
	// The read does not become a write.
	if err := rbac.Authorize(frank, "teams.update", authz.Resource{Kind: "team", ID: t1ID, Owner: &globalScope}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("teams.update = %v, want forbidden", err)
	}
}

// A caller with no bindings at all may still call the scoped lists; what
// comes back is filtered to nothing. `users` stays global-only.
func TestScopedListsAreOpenToAnyAuthenticatedCaller(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	stranger := ctxOf(actorOf(ids.New(), subjectsOf(ids.New())))

	for _, kind := range []string{"keys", "policies", "host-keys", "service-accounts",
		"policy-bindings", "role-bindings", "projects", "teams", "groups"} {
		if err := rbac.Authorize(stranger, kind+".list", authz.Resource{Kind: authz.Singular(kind)}); err != nil {
			t.Errorf("%s.list = %v, want allowed", kind, err)
		}
	}
	if err := rbac.Authorize(stranger, "users.list", authz.Resource{Kind: "user"}); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("users.list = %v, want forbidden", err)
	}
	if rbac.Visible(stranger, "key", "", *projectOwner(p1ID)) {
		t.Error("a binding-less caller sees a project's key")
	}
}
