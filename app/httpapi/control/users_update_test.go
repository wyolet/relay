package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

// updateUsers is an in-memory userWriter.
type updateUsers map[string]*user.User

func (u updateUsers) Get(_ context.Context, id string) (*user.User, error) { return u[id], nil }

func (u updateUsers) Upsert(_ context.Context, in *user.User) error {
	u[in.ID] = in
	return nil
}

func (u updateUsers) BumpTokenVersion(_ context.Context, id string) error {
	if u[id] == nil {
		return errors.New("no such user")
	}
	u[id].TokenVersion++
	return nil
}

type allowAuthz struct{}

func (allowAuthz) Authorize(context.Context, string, authz.Resource) error { return nil }

func disableUserInput(id string, disabled bool) *userUpdateInput {
	in := &userUpdateInput{ID: id}
	in.Body.Disabled = &disabled
	return in
}

func TestDisablingAUserBumpsTheTokenVersion(t *testing.T) {
	users := updateUsers{"u-1": {ID: "u-1", Username: "alice", TokenVersion: 3}}

	out, err := updateUser(context.Background(), users, allowAuthz{}, disableUserInput("u-1", true))
	if err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if !out.Body.Disabled {
		t.Error("response reports the account as enabled")
	}
	if !users["u-1"].Disabled {
		t.Error("the stored row was not disabled")
	}
	if got := users["u-1"].TokenVersion; got != 4 {
		t.Fatalf("token version = %d, want 4 (bumped so minted tokens stop verifying)", got)
	}

	// A second disable changes nothing, so it must not invalidate tokens
	// minted after the first one.
	if _, err := updateUser(context.Background(), users, allowAuthz{}, disableUserInput("u-1", true)); err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if got := users["u-1"].TokenVersion; got != 4 {
		t.Fatalf("token version = %d after a no-op disable, want 4", got)
	}
}

func TestUpdatingRolesLeavesTheTokenVersionAlone(t *testing.T) {
	users := updateUsers{"u-1": {ID: "u-1", Username: "alice", TokenVersion: 3}}
	in := &userUpdateInput{ID: "u-1"}
	roles := []string{user.RoleAdmin}
	in.Body.Roles = &roles

	if _, err := updateUser(context.Background(), users, allowAuthz{}, in); err != nil {
		t.Fatalf("updateUser: %v", err)
	}
	if got := users["u-1"].Roles; len(got) != 1 || got[0] != user.RoleAdmin {
		t.Fatalf("roles = %v, want [admin]", got)
	}
	if got := users["u-1"].TokenVersion; got != 3 {
		t.Fatalf("token version = %d, want 3 (unchanged)", got)
	}
}

func TestUserUpdateAuthorizesAtTheGlobalScope(t *testing.T) {
	c := &captureAuthz{}
	users := updateUsers{"u-1": {ID: "u-1", Username: "alice"}}

	if _, err := updateUser(context.Background(), users, c, disableUserInput("u-1", true)); err == nil {
		t.Fatal("a forbidden caller updated the account")
	}
	if c.got.Owner == nil || c.got.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("authorized against owner %+v, want {system}", c.got.Owner)
	}
	if users["u-1"].Disabled {
		t.Error("the row was written despite the denial")
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
