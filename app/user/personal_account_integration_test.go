//go:build integration

package user_test

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/user"
)

// A user's personalAccount sits in a table of relay's beside the directory row: Get and List read it back, "" removes it, a user that does not exist reports false, and deleting the user takes it along.
func TestPersonalAccountRoundTrip(t *testing.T) {
	store, ctx := setupUserDB(t)
	id := meta.NewID()
	u := &user.User{ID: id, Username: "pa-" + id[len(id)-8:]}
	if err := store.Upsert(ctx, u); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	read := func() (string, string) {
		t.Helper()
		got, err := store.Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("Get: %v (%v)", err, got)
		}
		all, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, x := range all {
			if x.ID == id {
				return got.PersonalAccount, x.PersonalAccount
			}
		}
		t.Fatal("List lacks the user")
		return "", ""
	}
	for _, v := range []string{"off", "on", ""} {
		if ok, err := store.SetPersonalAccount(ctx, id, v); err != nil || !ok {
			t.Fatalf("SetPersonalAccount(%q) = %v, %v", v, ok, err)
		}
		if g, l := read(); g != v || l != v {
			t.Fatalf("after %q: Get %q, List %q", v, g, l)
		}
	}
	if ok, err := store.SetPersonalAccount(ctx, meta.NewID(), "off"); err != nil || ok {
		t.Fatalf("SetPersonalAccount on a missing user = %v, %v; want false", ok, err)
	}
	if _, err := store.SetPersonalAccount(ctx, id, "off"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	again := &user.User{ID: id, Username: u.Username}
	if err := store.Upsert(ctx, again); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, id) })
	if g, _ := read(); g != "" {
		t.Fatalf("a user re-created under the same id inherited %q", g)
	}
}
