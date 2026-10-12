package directory_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

func tenancy(t *testing.T, h *harness) (*directory.Team, *directory.Project, *directory.ServiceAccount, *directory.User) {
	t.Helper()
	team := &directory.Team{Meta: directory.Metadata{Name: "core"}}
	must(t, h.d.CreateTeam(ctx, team))
	p := &directory.Project{Meta: directory.Metadata{Name: "api"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, h.d.CreateProject(ctx, p))
	sa := &directory.ServiceAccount{Meta: directory.Metadata{Name: "ci"}, Spec: directory.ServiceAccountSpec{ProjectID: p.Meta.ID}}
	must(t, h.d.CreateServiceAccount(ctx, sa))
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))
	return team, p, sa, u
}

func TestRoles(t *testing.T) {
	h := newHarness(t)
	for name, rules := range map[string][]rbac.Rule{
		"no rules":     nil,
		"unknown kind": {{Kinds: []string{"documnets"}, Verbs: []string{"get"}}},
		"unknown verb": {{Kinds: []string{"documents"}, Verbs: []string{"updat"}}},
		"empty verbs":  {{Kinds: []string{"documents"}}},
		"empty entry":  {{Kinds: []string{""}, Verbs: []string{"get"}}},
	} {
		wantErr(t, name, h.d.CreateRole(ctx, &directory.Role{Meta: directory.Metadata{Name: "r"}, Spec: directory.RoleSpec{Rules: rules}}), directory.ErrInvalid)
	}
	r := &directory.Role{Meta: directory.Metadata{DisplayName: "Doc Reader"}, Spec: directory.RoleSpec{
		Rules: []rbac.Rule{{Kinds: []string{"*"}, Verbs: []string{"get", "list"}}}}}
	must(t, h.d.CreateRole(ctx, r))
	if r.Meta.Name != "doc-reader" || r.Meta.Owner.Kind != directory.OwnerSystem || !r.IsEnabled() {
		t.Fatalf("role %+v", r.Meta)
	}
	if got := r.RBAC(); got.ID != r.Meta.ID || len(got.Rules) != 1 {
		t.Fatalf("RBAC() = %+v", got)
	}

	team, p, _, u := tenancy(t, h)
	for _, scope := range []rbac.Scope{{Kind: directory.OwnerTeam, ID: team.Meta.ID}, {Kind: directory.OwnerProject, ID: p.Meta.ID}} {
		must(t, h.d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{DisplayName: "readers"},
			Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID, Scope: scope, Subjects: []directory.Subject{{Kind: directory.SubjectUser, ID: u.ID}}}}))
	}
	h.grants.calls = nil
	r.Spec.Rules = append(r.Spec.Rules, rbac.Rule{Kinds: []string{"documents"}, Verbs: []string{"update"}})
	must(t, h.d.UpdateRole(ctx, caller, r))
	scopes := []rbac.Scope{h.grants.calls[0].scope, h.grants.calls[1].scope}
	if len(h.grants.calls) != 2 || !slices.Contains(scopes, rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID}) ||
		!slices.Contains(scopes, rbac.Scope{Kind: directory.OwnerProject, ID: p.Meta.ID}) {
		t.Fatalf("update re-checked %+v, want both bound scopes", h.grants.calls)
	}
	h.grants.deny[r.Meta.ID] = true
	wantErr(t, "update the caller could not grant", h.d.UpdateRole(ctx, caller, r), authz.ErrForbidden)
	wantErr(t, "update a missing role", h.d.UpdateRole(ctx, caller, &directory.Role{Meta: directory.Metadata{ID: "nope"}}), directory.ErrNotFound)

	must(t, h.d.DeleteRole(ctx, r.Meta.ID))
	if bs, _ := h.s.RoleBindings.List(ctx); len(bs) != 0 {
		t.Fatalf("role delete left %d bindings", len(bs))
	}
}

func TestRoleBindings(t *testing.T) {
	h := newHarness(t)
	team, p, sa, u := tenancy(t, h)
	r := addRole(t, h, "reader")
	subjects := []directory.Subject{
		{Kind: directory.SubjectUser, ID: u.ID},
		{Kind: directory.SubjectServiceAccount, ID: sa.Meta.ID},
		{Kind: directory.SubjectGroup, Name: "idp-admins"},
		directory.AllAuthenticated,
	}
	b := &directory.RoleBinding{Meta: directory.Metadata{Name: "readers"}, Spec: directory.RoleBindingSpec{
		RoleID: r.Meta.ID, Scope: rbac.Scope{Kind: directory.OwnerProject, ID: p.Meta.ID}, Subjects: subjects}}
	must(t, h.d.CreateRoleBinding(ctx, caller, b))
	if b.Meta.Owner != (authz.Owner{Kind: directory.OwnerProject, ID: p.Meta.ID}) || !b.IsEnabled() {
		t.Fatalf("binding owner = %+v", b.Meta.Owner)
	}
	if !slices.Equal(h.grants.calls, []grantCall{{r.Meta.ID, b.Spec.Scope}}) {
		t.Fatalf("grant checks = %+v", h.grants.calls)
	}
	if got := directory.AllAuthenticated.Key(); got != "group:system:authenticated" {
		t.Fatalf("AllAuthenticated.Key() = %q", got)
	}
	if got := subjects[0].Key(); got != "user:"+u.ID {
		t.Fatalf("user subject key = %q", got)
	}

	b.Spec.Scope = rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID}
	h.grants.deny[r.Meta.ID] = true
	wantErr(t, "a grant the caller lacks", h.d.UpdateRoleBinding(ctx, caller, b), authz.ErrForbidden)
	stored, _ := h.s.RoleBindings.Get(ctx, b.Meta.ID)
	if stored.Spec.Scope.Kind != directory.OwnerProject {
		t.Fatal("a refused update was written")
	}
	must(t, h.d.UpdateRoleBinding(ctx, admin, b))

	bind := func(scope rbac.Scope, subs ...directory.Subject) *directory.RoleBinding {
		return &directory.RoleBinding{Meta: directory.Metadata{Name: "x"}, Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID, Scope: scope, Subjects: subs}}
	}
	everyone := directory.AllAuthenticated
	for name, bad := range map[string]*directory.RoleBinding{
		"missing role":      {Meta: directory.Metadata{Name: "x"}, Spec: directory.RoleBindingSpec{RoleID: "nope", Scope: directory.Global, Subjects: []directory.Subject{everyone}}},
		"no role":           {Meta: directory.Metadata{Name: "x"}, Spec: directory.RoleBindingSpec{Scope: directory.Global, Subjects: []directory.Subject{everyone}}},
		"no scope kind":     bind(rbac.Scope{}, everyone),
		"global with id":    bind(rbac.Scope{Kind: directory.OwnerSystem, ID: "x"}, everyone),
		"team without id":   bind(rbac.Scope{Kind: directory.OwnerTeam}, everyone),
		"missing team":      bind(rbac.Scope{Kind: directory.OwnerTeam, ID: "nope"}, everyone),
		"missing project":   bind(rbac.Scope{Kind: directory.OwnerProject, ID: "nope"}, everyone),
		"no subjects":       bind(directory.Global),
		"missing user":      bind(directory.Global, directory.Subject{Kind: directory.SubjectUser, ID: "nope"}),
		"missing account":   bind(directory.Global, directory.Subject{Kind: directory.SubjectServiceAccount, ID: "nope"}),
		"group with id":     bind(directory.Global, directory.Subject{Kind: directory.SubjectGroup, ID: "g"}),
		"user with name":    bind(directory.Global, directory.Subject{Kind: directory.SubjectUser, Name: "ada"}),
		"unknown kind":      bind(directory.Global, directory.Subject{Kind: "role", Name: "admin"}),
		"duplicate subject": bind(directory.Global, everyone, everyone),
	} {
		wantErr(t, name, h.d.CreateRoleBinding(ctx, admin, bad), directory.ErrInvalid)
	}
	must(t, h.d.CreateRoleBinding(ctx, admin, bind(rbac.Scope{Kind: "folder", ID: "f-1"}, everyone)))

	must(t, h.d.DeleteRoleBinding(ctx, b.Meta.ID))
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindRoleBinding, Op: directory.OpDelete, ID: b.Meta.ID}) {
		t.Fatalf("change = %+v", c)
	}
	wantErr(t, "update a missing binding", h.d.UpdateRoleBinding(ctx, admin, b), directory.ErrNotFound)

	h.grants.err = errors.New("source down")
	if err := h.d.CreateRoleBinding(ctx, admin, bind(directory.Global, everyone)); err == nil || errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("a failed grant lookup = %v, want the lookup error", err)
	}
}

// TestGrantsThroughEngine wires the real engine over the directory's own resolver: a project admin may bind inside the project but not at its team.
func TestGrantsThroughEngine(t *testing.T) {
	s := memstore.New().Stores()
	src := &bindingSource{s: s}
	engine := &rbac.Engine{Source: src, Scopes: directory.ScopeResolver{Hierarchy: directory.StoreHierarchy(s)}, Global: directory.Global}
	d, err := directory.New(directory.Config{Stores: s, Grants: engine, Kinds: []string{"*", "documents"}, Verbs: []string{"*", "get"}})
	must(t, err)

	team := &directory.Team{Meta: directory.Metadata{Name: "core"}}
	must(t, d.CreateTeam(ctx, team))
	p := &directory.Project{Meta: directory.Metadata{Name: "api"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, d.CreateProject(ctx, p))
	owner := &directory.User{Username: "owner"}
	must(t, d.CreateUser(ctx, nil, owner))
	all := &directory.Role{Meta: directory.Metadata{Name: "all"}, Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"*"}, Verbs: []string{"*"}}}}}
	reader := &directory.Role{Meta: directory.Metadata{Name: "reader"}, Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"documents"}, Verbs: []string{"get"}}}}}
	must(t, d.CreateRole(ctx, all))
	must(t, d.CreateRole(ctx, reader))
	projectScope := rbac.Scope{Kind: directory.OwnerProject, ID: p.Meta.ID}
	must(t, d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: "owners"},
		Spec: directory.RoleBindingSpec{RoleID: all.Meta.ID, Scope: projectScope, Subjects: []directory.Subject{{Kind: directory.SubjectUser, ID: owner.ID}}}}))

	p1 := &principal.Principal{ID: owner.ID, Kind: principal.KindUser, Subjects: []string{"user:" + owner.ID}}
	must(t, d.CreateRoleBinding(ctx, p1, &directory.RoleBinding{Meta: directory.Metadata{Name: "readers"},
		Spec: directory.RoleBindingSpec{RoleID: reader.Meta.ID, Scope: projectScope, Subjects: []directory.Subject{directory.AllAuthenticated}}}))
	wantErr(t, "binding at the team", d.CreateRoleBinding(ctx, p1, &directory.RoleBinding{Meta: directory.Metadata{Name: "team-readers"},
		Spec: directory.RoleBindingSpec{RoleID: reader.Meta.ID, Scope: rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID},
			Subjects: []directory.Subject{directory.AllAuthenticated}}}), authz.ErrForbidden)
	wantErr(t, "a global role on a user", d.UpdateUser(ctx, p1, &directory.User{ID: owner.ID, Username: "owner", Roles: []string{"reader"}}), authz.ErrForbidden)
}

// bindingSource is an rbac.Source over the stores, indexing subjects by Key.
type bindingSource struct{ s directory.Stores }

func (b *bindingSource) BindingsForSubject(ctx context.Context, subject string) ([]rbac.Binding, error) {
	all, err := b.s.RoleBindings.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []rbac.Binding
	for _, rb := range all {
		for _, sub := range rb.Spec.Subjects {
			if sub.Key() == subject {
				out = append(out, rbac.Binding{RoleID: rb.Spec.RoleID, Scope: rb.Spec.Scope})
			}
		}
	}
	return out, nil
}

func (b *bindingSource) Role(ctx context.Context, id string) (rbac.Role, bool, error) {
	r, err := b.s.Roles.Get(ctx, id)
	if err != nil || r == nil {
		return rbac.Role{}, false, err
	}
	return r.RBAC(), true, nil
}
