package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/user"
)

// A group's personalAccount is an admin's, whoever owns the group: its owner may edit the rest, and a write of theirs that leaves the value out keeps it.
func TestGroupPersonalAccountIsAdminOnly(t *testing.T) {
	owner := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-owner"})
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-admin", Roles: []string{user.RoleAdmin}})
	stored := &group.Group{Spec: group.Spec{PersonalAccount: "off"}}

	var se huma.StatusError
	if err := guardGroupPersonalAccount(owner, scopedAllow{}, stored, &group.Group{Spec: group.Spec{PersonalAccount: "on"}}); !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
		t.Fatalf("owner turning it on = %v, want 403", err)
	}
	if err := guardGroupPersonalAccount(owner, scopedAllow{}, nil, &group.Group{Spec: group.Spec{PersonalAccount: "off"}}); !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
		t.Fatalf("owner creating a group with it = %v, want 403", err)
	}
	kept := &group.Group{}
	if err := guardGroupPersonalAccount(owner, scopedAllow{}, stored, kept); err != nil || kept.Spec.PersonalAccount != "off" {
		t.Fatalf("owner's edit leaving it out = %v, value %q; want the stored off kept", err, kept.Spec.PersonalAccount)
	}
	cleared := &group.Group{}
	if err := guardGroupPersonalAccount(admin, scopedAllow{}, stored, cleared); err != nil || cleared.Spec.PersonalAccount != "" {
		t.Fatalf("admin clearing it = %v, value %q; want it cleared", err, cleared.Spec.PersonalAccount)
	}
}
