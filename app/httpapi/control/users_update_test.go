package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/principal"
)

// updateUsers is an in-memory userWriter: a directory over the auth module's memstore, read back as relay users.
type updateUsers struct {
	t   *testing.T
	ms  *memstore.Store
	dir *directory.Directory
}

func newUpdateUsers(t *testing.T, rows ...*user.User) *updateUsers {
	t.Helper()
	ms := memstore.New()
	dir, err := directory.New(directory.Config{Stores: ms.Stores(), Grants: grantChecker{authz: allowAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	u := &updateUsers{t: t, ms: ms, dir: dir}
	for _, r := range rows {
		u.put(r)
	}
	return u
}

// put stores r as it is, token version included.
func (u *updateUsers) put(r *user.User) {
	u.t.Helper()
	ctx := context.Background()
	s := u.ms.Stores().Users
	existing, _ := s.Get(ctx, r.ID)
	if err := s.Upsert(ctx, r.ToDirectory()); err != nil {
		u.t.Fatal(err)
	}
	have := 1
	if existing != nil {
		have = existing.TokenVersion
	}
	for ; have < r.TokenVersion; have++ {
		if err := s.BumpTokenVersion(ctx, r.ID); err != nil {
			u.t.Fatal(err)
		}
	}
}

func (u *updateUsers) row(id string) *user.User {
	u.t.Helper()
	r, err := u.Get(context.Background(), id)
	if err != nil || r == nil {
		u.t.Fatalf("user %s: %v", id, err)
	}
	return r
}

func (u *updateUsers) List(ctx context.Context) ([]*user.User, error) {
	rows, err := u.ms.Stores().Users.List(ctx)
	out := make([]*user.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, user.FromDirectory(r))
	}
	return out, err
}

func (u *updateUsers) Get(ctx context.Context, id string) (*user.User, error) {
	r, err := u.ms.Stores().Users.Get(ctx, id)
	return user.FromDirectory(r), err
}

func (u *updateUsers) UpdateUser(ctx context.Context, p *principal.Principal, r *directory.User) error {
	return u.dir.UpdateUser(ctx, p, r)
}

func (u *updateUsers) DisableUser(ctx context.Context, id string) error {
	return u.dir.DisableUser(ctx, id)
}

func (u *updateUsers) EnableUser(ctx context.Context, id string) error {
	return u.dir.EnableUser(ctx, id)
}

type allowAuthz struct{}

func (allowAuthz) Authorize(context.Context, string, authz.Resource) error { return nil }

func disableUserInput(id string, disabled bool) *userUpdateInput {
	in := &userUpdateInput{ID: id}
	in.Body.Disabled = &disabled
	return in
}

func TestDisablingAUserBumpsTheTokenVersion(t *testing.T) {
	users := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice", TokenVersion: 3})

	out, err := updateUser(context.Background(), users, allowAuthz{}, disableUserInput("u-1", true))
	if err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if !out.Body.Disabled {
		t.Error("response reports the account as enabled")
	}
	if !users.row("u-1").Disabled {
		t.Error("the stored row was not disabled")
	}
	if got := users.row("u-1").TokenVersion; got != 4 {
		t.Fatalf("token version = %d, want 4 (bumped so minted tokens stop verifying)", got)
	}

	// A second disable changes nothing, so it must not invalidate tokens
	// minted after the first one.
	if _, err := updateUser(context.Background(), users, allowAuthz{}, disableUserInput("u-1", true)); err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if got := users.row("u-1").TokenVersion; got != 4 {
		t.Fatalf("token version = %d after a no-op disable, want 4", got)
	}
}

func TestUpdatingRolesLeavesTheTokenVersionAlone(t *testing.T) {
	users := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice", TokenVersion: 3})
	in := &userUpdateInput{ID: "u-1"}
	roles := []string{user.RoleAdmin}
	in.Body.Roles = &roles

	if _, err := updateUser(context.Background(), users, allowAuthz{}, in); err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if got := users.row("u-1").Roles; len(got) != 1 || got[0] != user.RoleAdmin {
		t.Fatalf("roles = %v, want [admin]", got)
	}
	if got := users.row("u-1").TokenVersion; got != 3 {
		t.Fatalf("token version = %d, want 3 (unchanged)", got)
	}
}

func TestUserUpdateAuthorizesAtTheGlobalScope(t *testing.T) {
	c := &captureAuthz{}
	users := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice"})

	if _, err := updateUser(context.Background(), users, c, disableUserInput("u-1", true)); err == nil {
		t.Fatal("a forbidden caller updated the account")
	}
	if c.got.Owner == nil || c.got.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("authorized against owner %+v, want {system}", c.got.Owner)
	}
	if users.row("u-1").Disabled {
		t.Error("the row was written despite the denial")
	}
}

// scopedAllow grants every action under a scoping authorizer, so the rules
// that look at the actor itself are the only ones left to refuse.
type scopedAllow struct{ allowAuthz }

func (scopedAllow) Visible(context.Context, string, string, meta.Owner) bool { return true }

func rolesInput(id string, roles ...string) *userUpdateInput {
	in := &userUpdateInput{ID: id}
	in.Body.Roles = &roles
	return in
}

// Holding users.update is not the right to hand out roles: only an admin
// changes them, or anyone with the verb could make themselves one.
func TestOnlyAnAdminChangesRoles(t *testing.T) {
	users := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice"})
	member := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	_, err := updateUser(member, users, scopedAllow{}, rolesInput("u-1", user.RoleAdmin))
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
		t.Fatalf("non-admin role change = %v, want 403", err)
	}
	if len(users.row("u-1").Roles) != 0 {
		t.Fatal("roles were written")
	}
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-0", Roles: []string{user.RoleAdmin}})
	if _, err := updateUser(admin, users, scopedAllow{}, rolesInput("u-1", user.RoleAdmin)); err != nil {
		t.Fatalf("admin role change: %v", err)
	}
}

// The deployment must keep one enabled admin: demoting or disabling the last
// one would leave nobody able to grant the role back.
func TestLastEnabledAdminCannotBeRemoved(t *testing.T) {
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	for name, in := range map[string]*userUpdateInput{
		"demote":  rolesInput("u-1"),
		"disable": disableUserInput("u-1", true),
	} {
		old := &user.User{ID: "u-2", Username: "old", Roles: []string{user.RoleAdmin}, Disabled: true}
		users := newUpdateUsers(t,
			&user.User{ID: "u-1", Username: "root", Roles: []string{user.RoleAdmin}},
			old,
		)
		_, err := updateUser(admin, users, scopedAllow{}, in)
		var se huma.StatusError
		if !errors.As(err, &se) || se.GetStatus() != http.StatusConflict {
			t.Fatalf("%s the last enabled admin = %v, want 409", name, err)
		}
		old.Disabled = false
		users.put(old)
		if _, err := updateUser(admin, users, scopedAllow{}, in); err != nil {
			t.Fatalf("%s with another enabled admin: %v", name, err)
		}
	}
}

func TestUserUpdateIsMountedOnTheUsersRoutes(t *testing.T) {
	c := &captureAuthz{}
	d := Deps{Authz: c, Users: &user.Store{}}
	h := usersHandlerWith(t, d)
	w := scopeReq(t, h, "alice", http.MethodPut, "/users/by-id/u-1", `{"disabled":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the route exists and authorizes)", w.Code)
	}
}
