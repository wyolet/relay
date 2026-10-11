package directory_test

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// A member acts under every binding naming the group, so adding one is checked like binding each of those roles, at each binding's scope; removing one is not.
func TestGroupMembershipIsAGrant(t *testing.T) {
	h := newHarness(t)
	team, _, _, ada := tenancy(t, h)
	bob := &directory.User{Username: "bob"}
	must(t, h.d.CreateUser(ctx, nil, bob))
	wide, narrow := addRole(t, h, "wide"), addRole(t, h, "narrow")
	teamScope := rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID}
	ops := directory.Subject{Kind: directory.SubjectGroup, Name: "ops"}
	bindGroup(t, h, "ops-wide", wide.Meta.ID, directory.Global, ops)
	bindGroup(t, h, "ops-narrow", narrow.Meta.ID, teamScope, ops)

	g := &directory.Group{Meta: directory.Metadata{Name: "ops"}}
	h.grants.calls = nil
	must(t, h.d.CreateGroup(ctx, caller, g))
	if len(h.grants.calls) != 0 {
		t.Fatalf("a group with no members was checked: %+v", h.grants.calls)
	}
	must(t, h.d.AddGroupMember(ctx, caller, g.Meta.ID, ada.ID))
	if want := []grantCall{{wide.Meta.ID, directory.Global}, {narrow.Meta.ID, teamScope}}; !sameCalls(h.grants.calls, want) {
		t.Fatalf("grant checks = %+v, want %+v", h.grants.calls, want)
	}

	h.grants.deny[wide.Meta.ID] = true
	wantErr(t, "add a member to a group bound to a role the caller lacks", h.d.AddGroupMember(ctx, caller, g.Meta.ID, bob.ID), authz.ErrForbidden)
	got, _ := h.s.Groups.Get(ctx, g.Meta.ID)
	wantErr(t, "update adding a member", h.d.UpdateGroup(ctx, caller, withMembers(got, ada.ID, bob.ID)), authz.ErrForbidden)
	if got, _ := h.s.Groups.Get(ctx, g.Meta.ID); !slices.Equal(got.Spec.MemberIDs, []string{ada.ID}) {
		t.Fatalf("a refused add was written: %v", got.Spec.MemberIDs)
	}
	must(t, h.d.CreateGroup(ctx, caller, &directory.Group{Meta: directory.Metadata{Name: "unbound"}, Spec: directory.GroupSpec{MemberIDs: []string{bob.ID}}}))
	must(t, h.d.AddGroupMember(ctx, admin, g.Meta.ID, bob.ID))

	h.grants.calls = nil
	must(t, h.d.RemoveGroupMember(ctx, g.Meta.ID, bob.ID))
	got, _ = h.s.Groups.Get(ctx, g.Meta.ID)
	got.Meta.DisplayName = "Ops"
	must(t, h.d.UpdateGroup(ctx, caller, withMembers(got)))
	if len(h.grants.calls) != 0 {
		t.Fatalf("removing members was checked: %+v", h.grants.calls)
	}
}

// Bindings name a group by name, so a group that takes a bound name, or is created under one, hands its members those bindings.
func TestGroupRenameAndCreateUnderABoundName(t *testing.T) {
	h := newHarness(t)
	_, _, _, ada := tenancy(t, h)
	wide := addRole(t, h, "wide")
	bindGroup(t, h, "admins", wide.Meta.ID, directory.Global, directory.Subject{Kind: directory.SubjectGroup, Name: "admins"})
	h.grants.deny[wide.Meta.ID] = true

	eng := &directory.Group{Meta: directory.Metadata{Name: "eng"}, Spec: directory.GroupSpec{MemberIDs: []string{ada.ID}}}
	must(t, h.d.CreateGroup(ctx, caller, eng))
	eng.Meta.Name = "admins"
	wantErr(t, "rename into a bound name", h.d.UpdateGroup(ctx, caller, eng), authz.ErrForbidden)
	wantErr(t, "create under a bound name", h.d.CreateGroup(ctx, caller,
		&directory.Group{Meta: directory.Metadata{Name: "admins"}, Spec: directory.GroupSpec{MemberIDs: []string{ada.ID}}}), authz.ErrForbidden)

	no := false
	off := &directory.Group{Meta: directory.Metadata{Name: "admins"}, Spec: directory.GroupSpec{Enabled: &no, MemberIDs: []string{ada.ID}}}
	must(t, h.d.CreateGroup(ctx, admin, off))
	yes := true
	off.Spec.Enabled = &yes
	wantErr(t, "re-enable a bound group", h.d.UpdateGroup(ctx, caller, off), authz.ErrForbidden)
}

// Through the real engine: a caller holding every permission inside one team may add members to a group bound only within that team, not to one bound globally.
func TestGroupMembershipThroughEngine(t *testing.T) {
	s := memstore.New().Stores()
	engine := &rbac.Engine{Source: &bindingSource{s: s}, Scopes: directory.ScopeResolver{Hierarchy: directory.StoreHierarchy(s)}, Global: directory.Global}
	d, err := directory.New(directory.Config{Stores: s, Grants: engine, Kinds: []string{"*", "documents"}, Verbs: []string{"*", "get"}})
	must(t, err)

	team := &directory.Team{Meta: directory.Metadata{Name: "alpha"}}
	must(t, d.CreateTeam(ctx, team))
	lead, dev := &directory.User{Username: "lead"}, &directory.User{Username: "dev"}
	must(t, d.CreateUser(ctx, nil, lead))
	must(t, d.CreateUser(ctx, nil, dev))
	all := &directory.Role{Meta: directory.Metadata{Name: "all"}, Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"*"}, Verbs: []string{"*"}}}}}
	must(t, d.CreateRole(ctx, all))
	teamScope := rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID}
	bind := func(name string, scope rbac.Scope, sub directory.Subject) {
		must(t, d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: name},
			Spec: directory.RoleBindingSpec{RoleID: all.Meta.ID, Scope: scope, Subjects: []directory.Subject{sub}}}))
	}
	bind("alpha-leads", teamScope, directory.Subject{Kind: directory.SubjectUser, ID: lead.ID})
	bind("alpha-ops", teamScope, directory.Subject{Kind: directory.SubjectGroup, Name: "alpha-ops"})
	bind("admins", directory.Global, directory.Subject{Kind: directory.SubjectGroup, Name: "admins"})
	teamOps, admins := &directory.Group{Meta: directory.Metadata{Name: "alpha-ops"}}, &directory.Group{Meta: directory.Metadata{Name: "admins"}}
	must(t, d.CreateGroup(ctx, admin, teamOps))
	must(t, d.CreateGroup(ctx, admin, admins))

	p := &principal.Principal{ID: lead.ID, Kind: principal.KindUser, Subjects: []string{"user:" + lead.ID}}
	must(t, d.AddGroupMember(ctx, p, teamOps.Meta.ID, dev.ID))
	wantErr(t, "a team lead adding to a globally bound group", d.AddGroupMember(ctx, p, admins.Meta.ID, dev.ID), authz.ErrForbidden)
	must(t, d.AddGroupMember(ctx, admin, admins.Meta.ID, dev.ID))
}

func bindGroup(t *testing.T, h *harness, name, roleID string, scope rbac.Scope, sub directory.Subject) {
	t.Helper()
	must(t, h.d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: name},
		Spec: directory.RoleBindingSpec{RoleID: roleID, Scope: scope, Subjects: []directory.Subject{sub}}}))
}

func withMembers(g *directory.Group, ids ...string) *directory.Group {
	c := *g
	c.Spec.MemberIDs = ids
	return &c
}

func sameCalls(got, want []grantCall) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}
