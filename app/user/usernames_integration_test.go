//go:build integration

package user_test

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

func TestUsernamesNamesOnlyExistingUsers(t *testing.T) {
	store, ctx := setupUserDB(t)

	present := &user.User{ID: meta.NewID(), Username: "named-" + meta.NewID()[:8]}
	if err := store.Upsert(ctx, present); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, present.ID) })

	got, err := store.Usernames(ctx, []string{present.ID, meta.NewID()})
	if err != nil {
		t.Fatalf("Usernames: %v", err)
	}
	if want := map[string]string{present.ID: present.Username}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Usernames = %v, want %v", got, want)
	}
	if got, err := store.Usernames(ctx, nil); err != nil || got != nil {
		t.Fatalf("Usernames(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}
