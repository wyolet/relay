package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/user"
)

// A live session carries the roles and state it logged in with, so a role change, a disable and a re-enable through the users endpoint each end the account's sessions; an edit that changes nothing ends none.
func TestUserUpdateEndsSessions(t *testing.T) {
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	users := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice"})
	for _, step := range []struct {
		name string
		in   *userUpdateInput
		ends bool
	}{
		{"role change", rolesInput("u-1", "viewer"), true},
		{"same roles", rolesInput("u-1", "viewer"), false},
		{"disable", disableUserInput("u-1", true), true},
		{"enable", disableUserInput("u-1", false), true},
		{"role change and disable", func() *userUpdateInput {
			in := rolesInput("u-1")
			in.Body.Disabled = disableUserInput("u-1", true).Body.Disabled
			return in
		}(), true},
	} {
		users.ended.ids = nil
		if _, err := updateUser(admin, users, scopedAllow{}, step.in); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := len(users.ended.ids) == 1 && users.ended.ids[0] == "u-1"; got != step.ends {
			t.Fatalf("%s: sessions ended for %v, want ended = %v", step.name, users.ended.ids, step.ends)
		}
	}
}

// racingUsers runs between once, right after the handler reads the account, as a concurrent writer would.
type racingUsers struct {
	*updateUsers
	between func()
}

func (r *racingUsers) Get(ctx context.Context, id string) (*user.User, error) {
	u, err := r.updateUsers.Get(ctx, id)
	if r.between != nil {
		run := r.between
		r.between = nil
		run()
	}
	return u, err
}

// A change another writer makes between the handler's read and its write is a conflict for the caller to retry, not overwritten: a role edit must not re-enable an account disabled meanwhile.
func TestUserUpdateRefusesAConcurrentChange(t *testing.T) {
	base := newUpdateUsers(t, &user.User{ID: "u-1", Username: "alice"})
	users := &racingUsers{updateUsers: base, between: func() {
		if err := base.DisableUser(context.Background(), "u-1"); err != nil {
			t.Fatal(err)
		}
	}}
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	_, err := updateUser(admin, users, scopedAllow{}, rolesInput("u-1", "viewer"))
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != http.StatusConflict {
		t.Fatalf("an update over a concurrent change = %v, want 409", err)
	}
	if got := base.row("u-1"); !got.Disabled || len(got.Roles) != 0 {
		t.Fatalf("the refused update was written: %+v", got)
	}
}
