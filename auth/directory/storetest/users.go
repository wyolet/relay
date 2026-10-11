package storetest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/directory"
)

func testUsers(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	u := s.Users

	got, err := u.Get(ctx, f.user.ID)
	must(t, err)
	sameUser(t, "get", got, f.user)
	if got.TokenVersion != 1 {
		t.Fatalf("new user token version = %d, want 1", got.TokenVersion)
	}
	got, err = u.ByUsername(ctx, "ada")
	must(t, err)
	sameUser(t, "by username", got, f.user)
	got, err = u.ByIdentity(ctx, f.user.Identity)
	must(t, err)
	sameUser(t, "by identity", got, f.user)

	for name, lookup := range map[string]func() (*directory.User, error){
		"get":         func() (*directory.User, error) { return u.Get(ctx, directory.NewID()) },
		"by username": func() (*directory.User, error) { return u.ByUsername(ctx, "nobody") },
		"by identity": func() (*directory.User, error) {
			return u.ByIdentity(ctx, directory.Identity{Issuer: "https://idp.example.com", Subject: "other"})
		},
	} {
		if got, err := lookup(); err != nil || got != nil {
			t.Fatalf("%s of a missing user = %v, %v; want nil, nil", name, got, err)
		}
	}

	must(t, u.BumpTokenVersion(ctx, f.user.ID))
	f.user.Disabled, f.user.Email, f.user.Roles = true, "", nil
	f.user.TokenVersion = 99
	must(t, u.Upsert(ctx, f.user))
	got, err = u.Get(ctx, f.user.ID)
	must(t, err)
	if got.TokenVersion != 2 {
		t.Fatalf("token version after bump and upsert = %d, want 2: upsert must leave it alone", got.TokenVersion)
	}
	sameUser(t, "after update", got, f.user)
	must(t, u.BumpTokenVersion(ctx, directory.NewID()))

	local := &directory.User{ID: directory.NewID(), Username: "bob"}
	must(t, u.Upsert(ctx, local))
	got, err = u.Get(ctx, local.ID)
	must(t, err)
	if !got.Identity.IsZero() || got.Email != "" || got.PasswordHash != "" {
		t.Fatalf("local user read back with %+v", got)
	}
	if got, err := u.ByIdentity(ctx, directory.Identity{}); err != nil || got != nil {
		t.Fatalf("by zero identity = %v, %v; want nil, nil", got, err)
	}

	list, err := u.List(ctx)
	must(t, err)
	if len(list) != 2 || list[0].ID != f.user.ID || list[1].ID != local.ID {
		t.Fatalf("list is not oldest first: %+v", list)
	}

	dupName := &directory.User{ID: directory.NewID(), Username: "ada"}
	wantErr(t, "duplicate username", u.Upsert(ctx, dupName), directory.ErrConflict)
	dupEmail := &directory.User{ID: directory.NewID(), Username: "eve", Email: "bob@example.com"}
	local.Email = "bob@example.com"
	must(t, u.Upsert(ctx, local))
	wantErr(t, "duplicate email", u.Upsert(ctx, dupEmail), directory.ErrConflict)
	dupIdentity := &directory.User{ID: directory.NewID(), Username: "eve", Identity: f.user.Identity}
	wantErr(t, "duplicate identity", u.Upsert(ctx, dupIdentity), directory.ErrConflict)

	must(t, u.Delete(ctx, local.ID))
	must(t, u.Delete(ctx, local.ID))
	if got, err := u.Get(ctx, local.ID); err != nil || got != nil {
		t.Fatalf("deleted user = %v, %v", got, err)
	}
}

func sameUser(t *testing.T, what string, got, want *directory.User) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: not found", what)
	}
	g, w := *got, *want
	g.TokenVersion, g.CreatedAt, g.UpdatedAt = 0, time.Time{}, time.Time{}
	w.TokenVersion, w.CreatedAt, w.UpdatedAt = 0, time.Time{}, time.Time{}
	if w.Roles == nil {
		w.Roles = []string{}
	}
	// A row written with only a primary identity reads back holding it.
	if w.Identities == nil && !w.Identity.IsZero() {
		w.Identities = []directory.UserIdentity{{Identity: w.Identity}}
	}
	g.Identities, w.Identities = identityKeys(g.Identities), identityKeys(w.Identities)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s:\n got  %+v\n want %+v", what, g, w)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("%s: store did not set timestamps", what)
	}
	for _, x := range got.Identities {
		if x.CreatedAt.IsZero() {
			t.Fatalf("%s: store did not set the identity timestamp: %+v", what, x)
		}
	}
}

// identityKeys drops the store-set timestamps so identities compare by value.
func identityKeys(ids []directory.UserIdentity) []directory.UserIdentity {
	if len(ids) == 0 {
		return nil
	}
	out := make([]directory.UserIdentity, len(ids))
	for i, x := range ids {
		out[i] = directory.UserIdentity{Identity: x.Identity, ExternalID: x.ExternalID}
	}
	return out
}
