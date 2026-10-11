package manifest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

const aliceHash = "$2a$10$CwTycUXWue0Thq9StjUM0uJ8.6Y3bOS2jvUJFXJ3m/BS/YwgMm9aW"

// A password and an identity are set outside manifests too, so a document that leaves them out keeps them, and only an explicit empty value clears them.
func TestUserPasswordAndIdentityAreKeptUnlessCleared(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	users := e.store.Stores().Users
	e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\n"+
		"spec: {password: {hash: \""+aliceHash+"\"}, identity: {issuer: \"https://idp\", subject: a1}}\n", manifest.Options{})

	p := e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {email: a@example.com}\n", manifest.Options{})
	wantLines(t, actions(p), []string{"update User/alice spec.email"})
	u, _ := users.ByUsername(ctx, "alice")
	if u.PasswordHash != aliceHash || u.Identity != (directory.Identity{Issuer: "https://idp", Subject: "a1"}) {
		t.Fatalf("omitted fields were not kept: %+v", u)
	}

	// The identity goes first: with no password left, removing it would leave the account no way to sign in.
	for _, empty := range []string{"null", `""`, "{}"} {
		e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \""+aliceHash+"\"}, identity: {issuer: i, subject: s}}\n", manifest.Options{})
		p := e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {identity: "+empty+"}\n", manifest.Options{})
		wantLines(t, actions(p), []string{"update User/alice spec.identity"})
		p = e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: "+empty+", identity: "+empty+"}\n", manifest.Options{})
		wantLines(t, actions(p), []string{"update User/alice spec.password"})
		u, _ := users.ByUsername(ctx, "alice")
		if u.PasswordHash != "" || !u.Identity.IsZero() || u.Identities != nil {
			t.Errorf("%s did not clear: %+v", empty, u)
		}
	}

	e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \""+aliceHash+"\"}, identity: {issuer: i, subject: s}}\n", manifest.Options{})
	p = e.plan(t, e.parse(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: null, identity: null}\n"), manifest.Options{})
	if _, err := p.Apply(ctx, e.dir, admin); !errors.Is(err, directory.ErrInvalid) {
		t.Fatalf("clearing the password and the last identity at once: err = %v, want ErrInvalid", err)
	}
	if u, _ := users.ByUsername(ctx, "alice"); len(u.Identities) != 1 {
		t.Fatalf("a refused clear removed identities: %+v", u)
	}
}

// identity: null means the account has no identities, so every one goes in the first apply and the second has nothing to change; leaving identity out keeps them all.
func TestUserIdentityClearRemovesEveryIdentityOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	users := e.store.Stores().Users
	withIdentity := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \"" + aliceHash + "\"}, identity: {issuer: \"https://idp\", subject: a1}}\n"
	e.apply(t, withIdentity, manifest.Options{})
	u, _ := users.ByUsername(ctx, "alice")
	for _, sub := range []string{"a2", "a3"} {
		if err := e.dir.AddIdentity(ctx, u.ID, directory.UserIdentity{Identity: directory.Identity{Issuer: "https://idp", Subject: sub}}); err != nil {
			t.Fatal(err)
		}
	}

	omitted := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \"" + aliceHash + "\"}}\n"
	wantLines(t, actions(e.apply(t, omitted, manifest.Options{})), []string{"unchanged User/alice"})
	if u, _ := users.ByUsername(ctx, "alice"); len(u.Identities) != 3 {
		t.Fatalf("an omitted identity changed the identities: %+v", u.Identities)
	}

	cleared := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \"" + aliceHash + "\"}, identity: null}\n"
	wantLines(t, actions(e.apply(t, cleared, manifest.Options{})), []string{"update User/alice spec.identity"})
	if u, _ := users.ByUsername(ctx, "alice"); !u.Identity.IsZero() || u.Identities != nil {
		t.Fatalf("after the first apply: %+v", u)
	}
	wantLines(t, actions(e.apply(t, cleared, manifest.Options{})), []string{"unchanged User/alice"})
}

func TestUserSpecReportsAnEnvPassword(t *testing.T) {
	docs := newEnv(t).parse(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: a}\nspec: {password: {valueFrom: {env: P}}}\n"+
		"---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: b}\nspec: {password: {hash: \""+aliceHash+"\"}}\n")
	if !docs[0].User.UsesEnv() || docs[1].User.UsesEnv() {
		t.Errorf("UsesEnv = %v, %v", docs[0].User.UsesEnv(), docs[1].User.UsesEnv())
	}
}
