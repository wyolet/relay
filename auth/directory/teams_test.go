package directory_test

import (
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// A binding scoped to a deleted team or project would apply to a later row given that id, so it goes with the delete; a team's delete takes the bindings of its projects too.
func TestDeletingAScopeDeletesItsBindings(t *testing.T) {
	h := newHarness(t)
	team, p, _, u := tenancy(t, h)
	r := addRole(t, h, "reader")
	bind := func(name string, scope rbac.Scope) *directory.RoleBinding {
		b := &directory.RoleBinding{Meta: directory.Metadata{Name: name}, Spec: directory.RoleBindingSpec{
			RoleID: r.Meta.ID, Scope: scope, Subjects: []directory.Subject{{Kind: directory.SubjectUser, ID: u.ID}}}}
		must(t, h.d.CreateRoleBinding(ctx, admin, b))
		return b
	}
	web := &directory.Project{Meta: directory.Metadata{Name: "web"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, h.d.CreateProject(ctx, web))
	atWeb := bind("at-web", rbac.Scope{Kind: directory.OwnerProject, ID: web.Meta.ID})
	atProject := bind("at-api", rbac.Scope{Kind: directory.OwnerProject, ID: p.Meta.ID})
	atTeam := bind("at-team", rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID})
	global := bind("global", directory.Global)
	exists := func(b *directory.RoleBinding) bool {
		got, err := h.s.RoleBindings.Get(ctx, b.Meta.ID)
		must(t, err)
		return got != nil
	}

	must(t, h.d.DeleteProject(ctx, web.Meta.ID))
	if exists(atWeb) || !exists(atProject) || !exists(atTeam) {
		t.Fatal("a project delete must take only its own bindings")
	}
	must(t, h.d.DeleteTeam(ctx, team.Meta.ID))
	if exists(atProject) || exists(atTeam) {
		t.Fatal("a team delete left a binding at the team or one of its projects")
	}
	if !exists(global) {
		t.Fatal("a team delete took a global binding")
	}
}
