package directory_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

func provisioner(t *testing.T, h *harness, source string) *directory.Provisioner {
	t.Helper()
	p, err := h.d.Provisioner(source)
	must(t, err)
	return p
}

func wantForeign(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, directory.ErrNotFound) || !errors.Is(err, directory.ErrForeign) {
		t.Fatalf("%s: err = %v, want not found and foreign", what, err)
	}
}

func TestProvisionerNeedsASource(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.Provisioner(" "); !errors.Is(err, directory.ErrInvalid) {
		t.Fatalf("blank source: %v", err)
	}
}

func TestProvisionerStampsItsSourceAndSetsNoPrivilege(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	u, err := p.CreateUser(ctx, directory.ProvisionedUser{Username: "ada@example.com", Email: "ada@example.com", ExternalID: "obj-1"}, false)
	must(t, err)
	stored, _ := h.s.Users.Get(ctx, u.ID)
	if stored.External != (directory.External{Source: "entra", ID: "obj-1"}) || len(stored.Roles) != 0 || stored.PasswordHash != "" || !stored.Identity.IsZero() {
		t.Fatalf("stored %+v", stored)
	}

	// An operator's edits to the account survive the source's update.
	stored.Roles = []string{"reader"}
	addRole(t, h, "reader")
	must(t, h.d.UpdateUser(ctx, admin, stored))
	if _, err := p.UpdateUser(ctx, u.ID, directory.ProvisionedUser{Username: "ada@example.com", Email: "new@example.com"}); err != nil {
		t.Fatal(err)
	}
	after, _ := h.s.Users.Get(ctx, u.ID)
	if after.Email != "new@example.com" || after.External.ID != "" || after.External.Source != "entra" || !slices.Equal(after.Roles, []string{"reader"}) {
		t.Fatalf("after update %+v", after)
	}
}

func TestProvisionerCannotTouchALocalUser(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	local := &directory.User{Username: "root"}
	must(t, h.d.CreateUser(ctx, nil, local))

	_, err := p.User(ctx, local.ID)
	wantForeign(t, "read", err)
	if all, _ := p.Users(ctx); len(all) != 0 {
		t.Fatalf("listed %d local users", len(all))
	}
	_, err = p.UpdateUser(ctx, local.ID, directory.ProvisionedUser{Username: "root"})
	wantForeign(t, "update", err)
	wantForeign(t, "disable", p.DisableUser(ctx, local.ID))
	wantForeign(t, "enable", p.EnableUser(ctx, local.ID))
	wantForeign(t, "delete", p.DeleteUser(ctx, local.ID))
	if u, _ := h.s.Users.Get(ctx, local.ID); u == nil || u.Disabled {
		t.Fatalf("local user after the attempts: %+v", u)
	}
}

func TestProvisionerCannotTouchAnotherSourcesRows(t *testing.T) {
	h := newHarness(t)
	entra, okta := provisioner(t, h, "entra"), provisioner(t, h, "okta")
	u, err := okta.CreateUser(ctx, directory.ProvisionedUser{Username: "bob"}, false)
	must(t, err)
	g, err := okta.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: "Ops", MemberIDs: []string{u.ID}})
	must(t, err)

	wantForeign(t, "disable another source's user", entra.DisableUser(ctx, u.ID))
	_, err = entra.Group(ctx, g.Meta.ID)
	wantForeign(t, "read another source's group", err)
	_, err = entra.UpdateGroup(ctx, g.Meta.ID, directory.ProvisionedGroup{DisplayName: "Ops"}, "")
	wantForeign(t, "update another source's group", err)
	wantForeign(t, "delete another source's group", entra.DeleteGroup(ctx, g.Meta.ID))

	// Nor may it put another source's account into its own group.
	mine, err := entra.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: "Mine"})
	must(t, err)
	_, err = entra.UpdateGroup(ctx, mine.Meta.ID, directory.ProvisionedGroup{DisplayName: "Mine", MemberIDs: []string{u.ID}}, "")
	wantErr(t, "add another source's user", err, directory.ErrInvalid)
}

func TestProvisionerRefusesALocalGroup(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	local := &directory.Group{Meta: directory.Metadata{Name: "ops"}}
	must(t, h.d.CreateGroup(ctx, nil, local))
	_, err := p.UpdateGroup(ctx, local.Meta.ID, directory.ProvisionedGroup{DisplayName: "Ops"}, "")
	wantForeign(t, "update", err)
	wantForeign(t, "delete", p.DeleteGroup(ctx, local.Meta.ID))
}

func TestProvisionerMembershipOfItsOwnGroupSkipsTheGrantCheck(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	r := addRole(t, h, "writer")
	h.grants.deny[r.Meta.ID] = true
	g, err := p.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: "Writers", ExternalID: "g-1"})
	must(t, err)
	must(t, h.d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: "writers"},
		Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID, Scope: directory.Global,
			Subjects: []directory.Subject{{Kind: directory.SubjectGroup, Name: g.Meta.Name}}}}))
	u, err := p.CreateUser(ctx, directory.ProvisionedUser{Username: "ada"}, false)
	must(t, err)

	// The same change through the operator path is refused for a caller who could not grant the role.
	wantErr(t, "operator path", h.d.WithWriter(directory.Writer{Source: "entra"}).AddGroupMember(ctx, caller, g.Meta.ID, u.ID), authz.ErrForbidden)

	h.grants.calls = nil
	got, err := p.UpdateGroup(ctx, g.Meta.ID, directory.ProvisionedGroup{DisplayName: "Writers", ExternalID: "g-1", MemberIDs: []string{u.ID}}, g.Meta.ResourceVersion)
	must(t, err)
	if len(h.grants.calls) != 0 || !slices.Equal(got.Spec.MemberIDs, []string{u.ID}) {
		t.Fatalf("grant calls %v, members %v", h.grants.calls, got.Spec.MemberIDs)
	}
	if h.notes.last(t) != (directory.Change{Kind: directory.KindGroup, Op: directory.OpUpsert, ID: g.Meta.ID}) {
		t.Fatalf("change %+v", h.notes.last(t))
	}
}

func TestProvisionerGroupNamesAvoidBoundNames(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	r := addRole(t, h, "admin-ish")
	// A binding an operator made for an identity provider's "admins" claim has no stored group behind it.
	must(t, h.d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: "idp-admins"},
		Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID, Scope: directory.Global,
			Subjects: []directory.Subject{{Kind: directory.SubjectGroup, Name: "admins"}}}}))
	must(t, h.d.CreateGroup(ctx, nil, &directory.Group{Meta: directory.Metadata{Name: "ops"}}))

	for display, want := range map[string]string{"Admins": "admins-2", "Ops": "ops-2", "Sales Team": "sales-team"} {
		g, err := p.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: display})
		must(t, err)
		if g.Meta.Name != want || g.Meta.DisplayName != display {
			t.Errorf("%q named %q, want %q", display, g.Meta.Name, want)
		}
	}
	_, err := p.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: "!!!"})
	wantErr(t, "unsluggable", err, directory.ErrInvalid)
}

func TestProvisionerUpdateGroupIsConditional(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	g, err := p.CreateGroup(ctx, directory.ProvisionedGroup{DisplayName: "Ops"})
	must(t, err)
	stale := g.Meta.ResourceVersion
	_, err = p.UpdateGroup(ctx, g.Meta.ID, directory.ProvisionedGroup{DisplayName: "Ops 2"}, "")
	must(t, err)
	_, err = p.UpdateGroup(ctx, g.Meta.ID, directory.ProvisionedGroup{DisplayName: "Ops 3"}, stale)
	wantErr(t, "stale version", err, directory.ErrStaleResourceVersion)
	after, _ := h.s.Groups.Get(ctx, g.Meta.ID)
	if after.Meta.Name != "ops" || after.Meta.DisplayName != "Ops 2" {
		t.Fatalf("group after %+v", after.Meta)
	}
}

func TestProvisionerDisableEndsSessionsAndTokens(t *testing.T) {
	h := newHarness(t)
	p := provisioner(t, h, "entra")
	u, err := p.CreateUser(ctx, directory.ProvisionedUser{Username: "ada"}, false)
	must(t, err)
	before, _ := h.s.Users.Get(ctx, u.ID)
	must(t, p.DisableUser(ctx, u.ID))
	after, _ := h.s.Users.Get(ctx, u.ID)
	if !after.Disabled || after.TokenVersion <= before.TokenVersion || !slices.Contains(h.rev.ended, u.ID) {
		t.Fatalf("disable: disabled=%v version %d→%d ended=%v", after.Disabled, before.TokenVersion, after.TokenVersion, h.rev.ended)
	}
	must(t, p.EnableUser(ctx, u.ID))
	must(t, p.DeleteUser(ctx, u.ID))
	if gone, _ := h.s.Users.Get(ctx, u.ID); gone != nil {
		t.Fatal("delete left the account")
	}
}

// A provisioning source has no way to reach roles, bindings, teams, projects or service accounts: the provisioner has no operation on them.
func TestProvisionerHasNoOperationOnPrivilegeKinds(t *testing.T) {
	typ := reflect.TypeFor[*directory.Provisioner]()
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		for _, kind := range []string{"Role", "Binding", "Team", "Project", "ServiceAccount", "Identity", "Password"} {
			if strings.Contains(name, kind) {
				t.Errorf("Provisioner.%s reaches %s", name, kind)
			}
		}
	}
}
