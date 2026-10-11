package storetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

func testIdentities(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	u := s.Users
	user := func(id string) *directory.User {
		return fetch(t, func() (*directory.User, error) { return u.Get(ctx, id) })
	}
	primary := f.user.Identity
	second := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "o-1"}, ExternalID: "obj-1"}

	must(t, u.AddIdentity(ctx, f.user.ID, second))
	got := user(f.user.ID)
	wantIdentities(t, "after add", got, primary, directory.UserIdentity{Identity: primary}, second)
	byID, err := u.ByIdentity(ctx, second.Identity)
	must(t, err)
	if byID == nil || byID.ID != f.user.ID {
		t.Fatalf("by a second identity = %+v", byID)
	}
	list, err := u.List(ctx)
	must(t, err)
	wantIdentities(t, "list", list[0], primary, directory.UserIdentity{Identity: primary}, second)

	second.ExternalID = "obj-2"
	must(t, u.AddIdentity(ctx, f.user.ID, second))
	wantIdentities(t, "re-add", user(f.user.ID), primary, directory.UserIdentity{Identity: primary}, second)

	got.Identities = nil
	must(t, u.Upsert(ctx, got))
	wantIdentities(t, "upsert ignores identities", user(f.user.ID), primary, directory.UserIdentity{Identity: primary}, second)

	bob := &directory.User{ID: directory.NewID(), Username: "bob"}
	must(t, u.Upsert(ctx, bob))
	wantErr(t, "identity another user holds", u.AddIdentity(ctx, bob.ID, second), directory.ErrConflict)
	sameExternal := directory.UserIdentity{Identity: directory.Identity{Issuer: second.Issuer, Subject: "o-2"}, ExternalID: "obj-2"}
	wantErr(t, "external id taken within the issuer", u.AddIdentity(ctx, bob.ID, sameExternal), directory.ErrConflict)
	bob.Identity = second.Identity
	wantErr(t, "primary another user holds", u.Upsert(ctx, bob), directory.ErrConflict)
	bob.Identity = directory.Identity{}

	first := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "bob"}}
	must(t, u.AddIdentity(ctx, bob.ID, first))
	wantIdentities(t, "first identity becomes primary", user(bob.ID), first.Identity, first)

	must(t, u.RemoveIdentity(ctx, f.user.ID, primary))
	wantIdentities(t, "primary removed", user(f.user.ID), second.Identity, second)
	if got, err := u.ByIdentity(ctx, primary); err != nil || got != nil {
		t.Fatalf("by a removed identity = %v, %v", got, err)
	}
	must(t, u.RemoveIdentity(ctx, f.user.ID, primary))

	third := directory.Identity{Issuer: "https://idp.example.com", Subject: "sub-3"}
	got = user(f.user.ID)
	got.Identity = third
	must(t, u.Upsert(ctx, got))
	wantIdentities(t, "primary replaced", user(f.user.ID), third, directory.UserIdentity{Identity: third})

	fourth := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "o-4"}}
	must(t, u.AddIdentity(ctx, f.user.ID, second))
	must(t, u.AddIdentity(ctx, f.user.ID, fourth))
	got = user(f.user.ID)
	got.Identity = directory.Identity{}
	must(t, u.Upsert(ctx, got))
	wantIdentities(t, "cleared primary promotes the oldest remaining", user(f.user.ID), second.Identity, second, fourth)
	must(t, u.RemoveIdentity(ctx, f.user.ID, fourth.Identity))
	got = user(f.user.ID)
	got.Identity = directory.Identity{}
	must(t, u.Upsert(ctx, got))
	wantIdentities(t, "last identity cleared", user(f.user.ID), directory.Identity{})

	must(t, u.AddIdentity(ctx, f.user.ID, second))
	must(t, u.AddIdentity(ctx, f.user.ID, fourth))
	must(t, u.RemoveIdentities(ctx, f.user.ID))
	wantIdentities(t, "every identity removed", user(f.user.ID), directory.Identity{})
	if got, err := u.ByIdentity(ctx, fourth.Identity); err != nil || got != nil {
		t.Fatalf("by an identity removed with the rest = %v, %v", got, err)
	}

	must(t, u.Delete(ctx, f.user.ID))
	if got, err := u.ByIdentity(ctx, second.Identity); err != nil || got != nil {
		t.Fatalf("identity of a deleted user = %v, %v", got, err)
	}
	must(t, u.AddIdentity(ctx, bob.ID, second))
}

func testFirstIdentity(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	u := s.Users
	scim1 := &directory.User{ID: directory.NewID(), Username: "eve", External: directory.External{Source: "scim-1", ID: "obj-1"}}
	scim2 := &directory.User{ID: directory.NewID(), Username: "eve-2", External: directory.External{Source: "scim-2", ID: "obj-1"}}
	other := &directory.User{ID: directory.NewID(), Username: "mal", External: directory.External{Source: "scim-1", ID: "obj-2"}}
	for _, x := range []*directory.User{scim1, scim2, other} {
		must(t, u.Upsert(ctx, x))
	}

	got, err := u.ByExternalID(ctx, "obj-1")
	must(t, err)
	if len(got) != 2 || got[0].ID != scim1.ID || got[1].ID != scim2.ID {
		t.Fatalf("by external id = %+v, want eve and eve-2", got)
	}
	for _, id := range []string{"obj-3", ""} {
		if got, err := u.ByExternalID(ctx, id); err != nil || len(got) != 0 {
			t.Fatalf("by external id %q = %+v, %v", id, got, err)
		}
	}

	first := directory.Identity{Issuer: "https://idp.example.com", Subject: "eve"}
	must(t, u.AddFirstIdentity(ctx, scim1.ID, first))
	wantIdentities(t, "first identity", fetch(t, func() (*directory.User, error) { return u.Get(ctx, scim1.ID) }), first, directory.UserIdentity{Identity: first})
	if byID, err := u.ByIdentity(ctx, first); err != nil || byID == nil || byID.ID != scim1.ID {
		t.Fatalf("by the linked identity = %+v, %v", byID, err)
	}
	got, err = u.ByExternalID(ctx, "obj-1")
	must(t, err)
	if len(got) != 2 || len(got[0].Identities) != 1 {
		t.Fatalf("by external id after the link = %+v", got)
	}

	second := directory.Identity{Issuer: "https://idp.example.com", Subject: "eve-again"}
	wantErr(t, "a second first identity", u.AddFirstIdentity(ctx, scim1.ID, second), directory.ErrConflict)
	wantErr(t, "an account with a primary", u.AddFirstIdentity(ctx, f.user.ID, second), directory.ErrConflict)
	wantErr(t, "an identity another account holds", u.AddFirstIdentity(ctx, scim2.ID, first), directory.ErrConflict)
	if got := fetch(t, func() (*directory.User, error) { return u.Get(ctx, scim2.ID) }); !got.Identity.IsZero() || len(got.Identities) != 0 {
		t.Fatalf("refused link wrote an identity: %+v", got)
	}
}

func wantIdentities(t *testing.T, what string, u *directory.User, primary directory.Identity, all ...directory.UserIdentity) {
	t.Helper()
	if u.Identity != primary {
		t.Fatalf("%s: primary = %+v, want %+v", what, u.Identity, primary)
	}
	if got := identityKeys(u.Identities); !reflect.DeepEqual(got, identityKeys(all)) {
		t.Fatalf("%s: identities = %+v, want %+v", what, got, all)
	}
}

func testExternal(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	ext := directory.External{Source: "scim-1", ID: "e-1"}

	f.user.External = ext
	must(t, s.Users.Upsert(ctx, f.user))
	sameUser(t, "user with an external id", fetch(t, func() (*directory.User, error) { return s.Users.Get(ctx, f.user.ID) }), f.user)
	dup := &directory.User{ID: directory.NewID(), Username: "eve", External: ext}
	wantErr(t, "duplicate user external id", s.Users.Upsert(ctx, dup), directory.ErrConflict)
	dup.External.Source = "scim-2"
	must(t, s.Users.Upsert(ctx, dup))
	sourceOnly := []*directory.User{
		{ID: directory.NewID(), Username: "a", External: directory.External{Source: "scim-1"}},
		{ID: directory.NewID(), Username: "b", External: directory.External{Source: "scim-1"}},
	}
	for _, x := range sourceOnly {
		must(t, s.Users.Upsert(ctx, x))
	}

	f.group.Spec.External = ext
	f.group.Meta.ResourceVersion = ""
	must(t, s.Groups.Upsert(ctx, f.group))
	sameRow(t, "group with an external id", fetch(t, get(s.Groups, f.group.Meta.ID)), f.group, groupMeta)
	other := &directory.Group{Meta: meta("other", directory.OwnerSystem, ""), Spec: directory.GroupSpec{External: ext}}
	wantErr(t, "duplicate group external id", s.Groups.Upsert(ctx, other), directory.ErrConflict)
	other.Spec.External.ID = ""
	must(t, s.Groups.Upsert(ctx, other))
}
