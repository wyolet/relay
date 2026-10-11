package directory_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

func TestManagedGroup(t *testing.T) {
	h := newHarness(t)
	u := &directory.User{Username: "ada"}
	must(t, h.d.CreateUser(ctx, nil, u))
	ext := directory.External{Source: "scim-1", ID: "g-1"}
	scim := h.d.WithWriter(directory.Writer{Source: "scim-1"})
	g := &directory.Group{Meta: directory.Metadata{Name: "ops"}, Spec: directory.GroupSpec{External: ext}}
	must(t, scim.CreateGroup(ctx, g))

	edit := func() *directory.Group {
		return &directory.Group{Meta: directory.Metadata{ID: g.Meta.ID, Name: "ops", DisplayName: "Ops"}}
	}
	err := h.d.UpdateGroup(ctx, edit())
	wantErr(t, "update by another writer", err, directory.ErrManaged)
	if me, ok := errors.AsType[*directory.ManagedError](err); !ok || me.Source != "scim-1" || me.Name != "ops" || me.Kind != "group" {
		t.Fatalf("managed error = %#v", err)
	}
	wantErr(t, "update by another source", h.d.WithWriter(directory.Writer{Source: "scim-2"}).UpdateGroup(ctx, edit()), directory.ErrManaged)
	wantErr(t, "add member", h.d.AddGroupMember(ctx, g.Meta.ID, u.ID), directory.ErrManaged)
	wantErr(t, "remove member", h.d.RemoveGroupMember(ctx, g.Meta.ID, u.ID), directory.ErrManaged)
	wantErr(t, "delete", h.d.DeleteGroup(ctx, g.Meta.ID), directory.ErrManaged)

	must(t, scim.AddGroupMember(ctx, g.Meta.ID, u.ID))
	must(t, scim.UpdateGroup(ctx, &directory.Group{Meta: directory.Metadata{ID: g.Meta.ID, Name: "ops"}, Spec: directory.GroupSpec{MemberIDs: []string{u.ID}}}))
	forced := h.d.WithWriter(directory.Writer{Force: true})
	must(t, forced.UpdateGroup(ctx, edit()))
	got, _ := h.s.Groups.Get(ctx, g.Meta.ID)
	if got.Spec.External != ext || got.Meta.DisplayName != "Ops" {
		t.Fatalf("a write that left external out changed it: %+v", got)
	}
	must(t, forced.DeleteGroup(ctx, g.Meta.ID))

	plain := &directory.Group{Meta: directory.Metadata{Name: "eng"}}
	must(t, h.d.CreateGroup(ctx, plain))
	must(t, h.d.AddGroupMember(ctx, plain.Meta.ID, u.ID))
	wantErr(t, "external id without a source", h.d.CreateGroup(ctx,
		&directory.Group{Meta: directory.Metadata{Name: "qa"}, Spec: directory.GroupSpec{External: directory.External{ID: "x"}}}), directory.ErrInvalid)
}
