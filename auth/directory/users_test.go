package directory_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/password"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

func addRole(t *testing.T, h *harness, name string) *directory.Role {
	t.Helper()
	r := &directory.Role{Meta: directory.Metadata{Name: name}, Spec: directory.RoleSpec{
		Rules: []rbac.Rule{{Kinds: []string{"documents"}, Verbs: []string{"get"}}}}}
	must(t, h.d.CreateRole(ctx, r))
	return r
}

func TestCreateUser(t *testing.T) {
	h := newHarness(t)
	hash, err := password.Hash("s3cret")
	must(t, err)
	u := &directory.User{Username: "ada", Email: "ada@example.com", PasswordHash: hash}
	must(t, h.d.CreateUser(ctx, nil, u))
	if u.ID == "" {
		t.Fatal("no id minted")
	}
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindUser, Op: directory.OpUpsert, ID: u.ID}) {
		t.Fatalf("change = %+v", c)
	}
	got, _ := h.s.Users.Get(ctx, u.ID)
	if got.TokenVersion != 1 || got.Status() != principal.StatusActive {
		t.Fatalf("stored %+v", got)
	}

	wantErr(t, "plain password", h.d.CreateUser(ctx, nil, &directory.User{Username: "bob", PasswordHash: "hunter2"}), directory.ErrInvalid)
	wantErr(t, "no username", h.d.CreateUser(ctx, nil, &directory.User{Username: " "}), directory.ErrInvalid)
	wantErr(t, "half an identity", h.d.CreateUser(ctx, nil, &directory.User{Username: "bob", Identity: directory.Identity{Subject: "s"}}), directory.ErrInvalid)
	wantErr(t, "separator in issuer", h.d.CreateUser(ctx, nil, &directory.User{Username: "bob",
		Identity: directory.Identity{Issuer: "a|b", Subject: "s"}}), directory.ErrInvalid)
	wantErr(t, "taken username", h.d.CreateUser(ctx, nil, &directory.User{Username: "ada"}), directory.ErrConflict)
	wantErr(t, "taken id", h.d.CreateUser(ctx, nil, &directory.User{ID: u.ID, Username: "bob"}), directory.ErrConflict)

	idp := directory.Identity{Issuer: "https://idp.example.com", Subject: "s-1"}
	must(t, h.d.CreateUser(ctx, nil, &directory.User{ID: "external-id", Username: "carol", Identity: idp}))
	wantErr(t, "taken identity", h.d.CreateUser(ctx, nil, &directory.User{Username: "dave", Identity: idp}), directory.ErrConflict)

	addRole(t, h, "viewer")
	wantErr(t, "roles without a principal", h.d.CreateUser(ctx, nil, &directory.User{Username: "erin", Roles: []string{"viewer"}}),
		authz.ErrUnauthenticated)
	must(t, h.d.CreateUser(ctx, caller, &directory.User{Username: "erin", Roles: []string{"viewer"}}))
}

// A password or identity the caller chose lets it sign in as the account: only an admin sets, changes or clears one, and only the product itself (a nil principal) registers an account with one.
func TestUserCredentialsNeedAnAdmin(t *testing.T) {
	h := newHarness(t)
	hash, err := password.Hash("s3cret")
	must(t, err)
	idp := directory.Identity{Issuer: "https://idp.example.com", Subject: "root"}
	root := &directory.User{Username: "root", PasswordHash: hash, Identity: idp}
	must(t, h.d.CreateUser(ctx, admin, root))
	other, err := password.Hash("mallory")
	must(t, err)

	wantErr(t, "a new account with a password", h.d.CreateUser(ctx, caller, &directory.User{Username: "m1", PasswordHash: hash}), authz.ErrForbidden)
	wantErr(t, "a new account with an identity", h.d.CreateUser(ctx, caller,
		&directory.User{Username: "m2", Identity: directory.Identity{Issuer: idp.Issuer, Subject: "m2"}}), authz.ErrForbidden)
	for name, edit := range map[string]func(u *directory.User){
		"password changed": func(u *directory.User) { u.PasswordHash = other },
		"password cleared": func(u *directory.User) { u.PasswordHash = "" },
		"identity changed": func(u *directory.User) { u.Identity.Subject = "mallory" },
		"identity cleared": func(u *directory.User) { u.Identity = directory.Identity{} },
	} {
		for _, p := range []*principal.Principal{caller, nil} {
			u, _ := h.s.Users.Get(ctx, root.ID)
			edit(u)
			wantErr(t, name, h.d.UpdateUser(ctx, p, u), authz.ErrForbidden)
		}
		if got, _ := h.s.Users.Get(ctx, root.ID); got.PasswordHash != hash || got.Identity != idp {
			t.Fatalf("%s: the refused change was written: %+v", name, got)
		}
	}
	u, _ := h.s.Users.Get(ctx, root.ID)
	u.PasswordHash = other
	must(t, h.d.UpdateUser(ctx, admin, u))
	must(t, h.d.CreateUser(ctx, nil, &directory.User{Username: "self", Identity: directory.Identity{Issuer: idp.Issuer, Subject: "self"}}))
}

func TestUpdateUserRoles(t *testing.T) {
	h := newHarness(t)
	viewer := addRole(t, h, "viewer")
	editor := addRole(t, h, "editor")
	u := &directory.User{Username: "ada", Roles: []string{"viewer"}}
	must(t, h.d.CreateUser(ctx, admin, u))
	h.grants.calls = nil

	u.Roles = []string{"editor"}
	must(t, h.d.UpdateUser(ctx, caller, u))
	want := []grantCall{{viewer.Meta.ID, directory.Global}, {editor.Meta.ID, directory.Global}}
	if !slices.Equal(h.grants.calls, want) {
		t.Fatalf("checked %+v, want the removed and the added role at the global scope", h.grants.calls)
	}
	if !slices.Equal(h.rev.ended, []string{u.ID}) {
		t.Fatalf("sessions ended for %v, want the user's", h.rev.ended)
	}

	h.grants.deny[viewer.Meta.ID] = true
	u.Roles = []string{"editor", "viewer"}
	wantErr(t, "adding a role the caller lacks", h.d.UpdateUser(ctx, caller, u), authz.ErrForbidden)
	stored, _ := h.s.Users.Get(ctx, u.ID)
	if !slices.Equal(stored.Roles, []string{"editor"}) {
		t.Fatalf("refused change was written: %v", stored.Roles)
	}

	u.Roles = []string{"editor", "admin"}
	wantErr(t, "a role with no row, as a non-admin", h.d.UpdateUser(ctx, caller, u), authz.ErrForbidden)
	wantErr(t, "a role with no row, unauthenticated", h.d.UpdateUser(ctx, nil, u), authz.ErrUnauthenticated)
	must(t, h.d.UpdateUser(ctx, admin, u))

	h.grants.calls, h.rev.ended = nil, nil
	u.Email = "ada@example.com"
	must(t, h.d.UpdateUser(ctx, nil, u))
	if len(h.grants.calls) != 0 || len(h.rev.ended) != 0 {
		t.Fatalf("an edit without a role change checked %v and ended %v", h.grants.calls, h.rev.ended)
	}
}

func TestDisableEnableUser(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))

	must(t, h.d.DisableUser(ctx, u.ID))
	got, _ := h.s.Users.Get(ctx, u.ID)
	if !got.Disabled || got.TokenVersion != 2 || got.Status() != principal.StatusDisabled {
		t.Fatalf("after disable: %+v", got)
	}
	if c := h.notes.last(t); c.Op != directory.OpDisable {
		t.Fatalf("change = %+v, want a disable", c)
	}
	if !slices.Equal(h.rev.ended, []string{u.ID}) {
		t.Fatalf("sessions ended for %v", h.rev.ended)
	}
	must(t, h.d.DisableUser(ctx, u.ID))
	if got, _ := h.s.Users.Get(ctx, u.ID); got.TokenVersion != 2 {
		t.Fatal("disabling a disabled account bumped the version")
	}

	must(t, h.d.EnableUser(ctx, u.ID))
	got, _ = h.s.Users.Get(ctx, u.ID)
	if got.Disabled || got.TokenVersion != 3 {
		t.Fatalf("after enable: %+v", got)
	}
	if c := h.notes.last(t); c.Op != directory.OpUpsert {
		t.Fatalf("change = %+v, want an upsert", c)
	}

	got.Disabled = true
	must(t, h.d.UpdateUser(ctx, nil, got))
	if again, _ := h.s.Users.Get(ctx, u.ID); again.TokenVersion != 4 {
		t.Fatalf("disable through UpdateUser: version %d", again.TokenVersion)
	}

	wantErr(t, "disable a missing user", h.d.DisableUser(ctx, "nope"), directory.ErrNotFound)
	wantErr(t, "update a missing user", h.d.UpdateUser(ctx, nil, &directory.User{ID: "nope", Username: "x"}), directory.ErrNotFound)

	h.rev.err = errors.New("kv down")
	if err := h.d.EnableUser(ctx, u.ID); err == nil {
		t.Fatal("a failed session revoke was not reported")
	}
}

// failingBump is a store whose separate token-version bump fails.
type failingBump struct{ directory.UserStore }

func (failingBump) BumpTokenVersion(context.Context, string) error { return errors.New("bump failed") }

// Disabling and re-enabling move the flag and the token version in one write: a failure can never leave the account enabled at the version its pre-disable tokens carry.
func TestDisableEnableBumpTheTokenVersionAtomically(t *testing.T) {
	s := memstore.New().Stores()
	s.Users = failingBump{s.Users}
	d, err := directory.New(directory.Config{Stores: s, Grants: &grants{deny: map[string]bool{}}})
	must(t, err)
	u := &directory.User{Username: "ada"}
	must(t, d.CreateUser(ctx, nil, u))
	minted, _ := s.Users.Get(ctx, u.ID)

	_ = d.DisableUser(ctx, u.ID)
	_ = d.EnableUser(ctx, u.ID)
	got, _ := s.Users.Get(ctx, u.ID)
	if !got.Disabled && got.TokenVersion == minted.TokenVersion {
		t.Fatalf("enabled at token version %d, which tokens minted before the disable carry", got.TokenVersion)
	}
}

func TestUpdateUserKeepsStoreFields(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada", PasswordHash: "legacy-plain"}
	must(t, h.s.Users.Upsert(ctx, &directory.User{ID: "u-1", Username: "ada", PasswordHash: "legacy-plain"}))
	u.ID = "u-1"
	u.Email = "ada@example.com"
	must(t, h.d.UpdateUser(ctx, nil, u))
	u.PasswordHash = "new-plain"
	wantErr(t, "a new plain password", h.d.UpdateUser(ctx, nil, u), directory.ErrInvalid)

	must(t, h.d.CreateUser(ctx, nil, &directory.User{Username: "bob"}))
	u.PasswordHash, u.Username = "legacy-plain", "bob"
	wantErr(t, "renaming onto a taken username", h.d.UpdateUser(ctx, nil, u), directory.ErrConflict)
}

func TestDeleteUser(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))
	g := &directory.Group{Meta: directory.Metadata{Name: "ops"}, Spec: directory.GroupSpec{MemberIDs: []string{u.ID}}}
	must(t, h.d.CreateGroup(ctx, caller, g))

	must(t, h.d.DeleteUser(ctx, u.ID))
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindUser, Op: directory.OpDelete, ID: u.ID}) {
		t.Fatalf("change = %+v", c)
	}
	if got, _ := h.s.Groups.Get(ctx, g.Meta.ID); got.Spec.MemberIDs != nil {
		t.Fatalf("deleted user still a member: %v", got.Spec.MemberIDs)
	}
	if !slices.Equal(h.rev.ended, []string{u.ID}) {
		t.Fatalf("sessions ended for %v", h.rev.ended)
	}
	wantErr(t, "delete again", h.d.DeleteUser(ctx, u.ID), directory.ErrNotFound)
}
