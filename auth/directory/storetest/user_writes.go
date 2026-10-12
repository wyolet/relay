package storetest

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

// testUserWrites covers the conditional upsert and the two narrow writes: none of them may write back a field its caller did not set.
func testUserWrites(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	u := s.Users

	read, err := u.Get(ctx, f.user.ID)
	must(t, err)
	stale := *read
	read.Email = "ada@corp.example.com"
	must(t, u.Upsert(ctx, read))
	stale.Roles = []string{"admin"}
	wantErr(t, "an upsert over a newer row", u.Upsert(ctx, &stale), directory.ErrStaleResourceVersion)
	stale.ResourceVersion = "not a version"
	wantErr(t, "an unparsable version", u.Upsert(ctx, &stale), directory.ErrStaleResourceVersion)
	got, err := u.Get(ctx, f.user.ID)
	must(t, err)
	if got.Email != "ada@corp.example.com" || slices.Contains(got.Roles, "admin") || got.ResourceVersion == read.ResourceVersion {
		t.Fatalf("after a refused stale upsert: %+v", got)
	}

	before := got.TokenVersion
	changed, err := u.SetDisabled(ctx, f.user.ID, true)
	must(t, err)
	got, _ = u.Get(ctx, f.user.ID)
	if !changed || !got.Disabled || got.TokenVersion != before+1 || got.Email != "ada@corp.example.com" || !slices.Equal(got.Roles, []string{"viewer"}) {
		t.Fatalf("after SetDisabled: changed %v, %+v", changed, got)
	}
	if changed, err := u.SetDisabled(ctx, f.user.ID, true); err != nil || changed {
		t.Fatalf("disabling a disabled account = %v, %v; want false", changed, err)
	}
	if changed, err := u.SetDisabled(ctx, directory.NewID(), true); err != nil || changed {
		t.Fatalf("disabling a missing account = %v, %v; want false", changed, err)
	}
	changed, err = u.SetDisabled(ctx, f.user.ID, false)
	must(t, err)
	if again, _ := u.Get(ctx, f.user.ID); !changed || again.Disabled || again.TokenVersion != before+2 {
		t.Fatalf("re-enabling must bump the token version again: %+v", again)
	}

	prov := &directory.User{ID: directory.NewID(), Username: "bob", Roles: []string{"viewer"}, Disabled: true,
		External: directory.External{Source: "scim-1", ID: "e-1"}}
	must(t, u.Upsert(ctx, prov))
	in := directory.ProvisionedUser{Username: "robert", Email: "robert@example.com", ExternalID: "e-2"}
	must(t, u.UpdateProvisioned(ctx, "scim-1", prov.ID, in))
	got, _ = u.Get(ctx, prov.ID)
	if got.Username != "robert" || got.Email != "robert@example.com" || got.External != (directory.External{Source: "scim-1", ID: "e-2"}) ||
		!got.Disabled || !slices.Equal(got.Roles, []string{"viewer"}) {
		t.Fatalf("after UpdateProvisioned: %+v", got)
	}
	wantErr(t, "another source's account", u.UpdateProvisioned(ctx, "scim-2", prov.ID, in), directory.ErrNotFound)
	wantErr(t, "a local account", u.UpdateProvisioned(ctx, "scim-1", f.user.ID, in), directory.ErrNotFound)
	wantErr(t, "a missing account", u.UpdateProvisioned(ctx, "scim-1", directory.NewID(), in), directory.ErrNotFound)
	in.Username = "ada"
	wantErr(t, "a taken username", u.UpdateProvisioned(ctx, "scim-1", prov.ID, in), directory.ErrConflict)

	// The edit and the removal land in one write: a stale version leaves both undone.
	with, _ := u.Get(ctx, f.user.ID)
	must(t, u.AddIdentity(ctx, f.user.ID, directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "o-8"}}))
	staleWith := *with
	staleWith.Email, staleWith.Identity, staleWith.PasswordHash = "gone@example.com", directory.Identity{}, "$2a$10$x"
	wantErr(t, "a stale upsert removing identities", u.UpsertRemovingIdentities(ctx, &staleWith), directory.ErrStaleResourceVersion)
	if got, _ := u.Get(ctx, f.user.ID); got.Email == "gone@example.com" || len(got.Identities) != 2 {
		t.Fatalf("a refused upsert removing identities wrote part of itself: %+v", got)
	}
	fresh, _ := u.Get(ctx, f.user.ID)
	fresh.Email, fresh.Identity, fresh.PasswordHash = "gone@example.com", directory.Identity{}, "$2a$10$x"
	must(t, u.UpsertRemovingIdentities(ctx, fresh))
	if got, _ := u.Get(ctx, f.user.ID); got.Email != "gone@example.com" || !got.Identity.IsZero() || got.Identities != nil || got.TokenVersion <= fresh.TokenVersion {
		t.Fatalf("after an upsert removing identities: %+v", got)
	}
	wantErr(t, "a primary kept while removing identities", u.UpsertRemovingIdentities(ctx, f.user), directory.ErrInvalid)

	extra := directory.Identity{Issuer: "https://other.example.com", Subject: "o-9"}
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"add first identity", func() error {
			return u.AddFirstIdentity(ctx, prov.ID, directory.Identity{Issuer: "https://idp.example.com", Subject: "bob"})
		}},
		{"add identity", func() error { return u.AddIdentity(ctx, prov.ID, directory.UserIdentity{Identity: extra}) }},
		{"remove identity", func() error { return u.RemoveIdentity(ctx, prov.ID, extra) }},
		{"remove identities", func() error { return u.RemoveIdentities(ctx, prov.ID) }},
	} {
		before, _ := u.Get(ctx, prov.ID)
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if after, _ := u.Get(ctx, prov.ID); after.TokenVersion != before.TokenVersion+1 {
			t.Fatalf("%s left the token version at %d, want %d", step.name, after.TokenVersion, before.TokenVersion+1)
		}
	}
}
