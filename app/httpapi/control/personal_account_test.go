package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/user"
)

func systemGroup(value string, members ...string) *group.Group {
	return &group.Group{Meta: meta.Metadata{ID: "g-1", Name: "pilots", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: group.Spec{MemberIDs: members, PersonalAccount: value}}
}

func wantErrStatus(t *testing.T, what string, err error, status int) {
	t.Helper()
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != status {
		t.Fatalf("%s = %v, want %d", what, err, status)
	}
}

// A group's personalAccount is an admin's: only an admin sets it, and while a group carries it only an admin changes its members, enabled flag, name or owner, or deletes it. A non-admin's edit that leaves the value out and changes nothing else keeps it.
func TestGroupPersonalAccountIsAdminOnly(t *testing.T) {
	member := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-member"})
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-admin", Roles: []string{user.RoleAdmin}})
	stored := systemGroup("off", "u-1")

	wantErrStatus(t, "non-admin turning it on", guardGroupPersonalAccount(member, scopedAllow{}, stored, systemGroup("on", "u-1")), http.StatusForbidden)
	wantErrStatus(t, "non-admin creating a group with it", guardGroupPersonalAccount(member, scopedAllow{}, nil, systemGroup("off")), http.StatusForbidden)
	wantErrStatus(t, "non-admin adding a member", guardGroupPersonalAccount(member, scopedAllow{}, stored, systemGroup("", "u-1", "u-2")), http.StatusForbidden)
	disabled := false
	off := systemGroup("", "u-1")
	off.Spec.Enabled = &disabled
	wantErrStatus(t, "non-admin disabling it", guardGroupPersonalAccount(member, scopedAllow{}, stored, off), http.StatusForbidden)
	wantErrStatus(t, "non-admin deleting it", guardGroupPersonalAccount(member, scopedAllow{}, stored, nil), http.StatusForbidden)

	kept := systemGroup("", "u-1")
	if err := guardGroupPersonalAccount(member, scopedAllow{}, stored, kept); err != nil || kept.Spec.PersonalAccount != "off" {
		t.Fatalf("non-admin edit leaving it out = %v, value %q; want the stored off kept", err, kept.Spec.PersonalAccount)
	}
	if err := guardGroupPersonalAccount(member, scopedAllow{}, systemGroup(""), systemGroup("", "u-9")); err != nil {
		t.Fatalf("non-admin editing a group without a value = %v, want it allowed", err)
	}
	if err := guardGroupPersonalAccount(admin, scopedAllow{}, stored, systemGroup("", "u-1", "u-2")); err != nil {
		t.Fatalf("admin clearing it and adding a member = %v", err)
	}
	if err := guardGroupPersonalAccount(admin, scopedAllow{}, stored, nil); err != nil {
		t.Fatalf("admin deleting it = %v", err)
	}
}

// personalAccount sits only on system groups: setting it on a user's group, or moving a group carrying it to a user, is refused even for an admin.
func TestGroupPersonalAccountOnlyOnSystemGroups(t *testing.T) {
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-admin", Roles: []string{user.RoleAdmin}})
	usersGroup := systemGroup("off")
	usersGroup.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-admin"}
	wantErrStatus(t, "a user's group with a value", guardGroupPersonalAccount(admin, scopedAllow{}, nil, usersGroup), http.StatusBadRequest)
	moved := systemGroup("off")
	moved.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-admin"}
	wantErrStatus(t, "moving a group carrying it to a user", guardGroupPersonalAccount(admin, scopedAllow{}, systemGroup("off"), moved), http.StatusBadRequest)
}
