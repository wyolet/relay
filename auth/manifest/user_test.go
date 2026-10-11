package manifest_test

import (
	"context"
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

	for _, clear := range []string{"{password: null, identity: null}", `{password: "", identity: ""}`, "{password: {}, identity: {}}"} {
		e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: {password: {hash: \""+aliceHash+"\"}, identity: {issuer: i, subject: s}}\n", manifest.Options{})
		p := e.apply(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec: "+clear+"\n", manifest.Options{})
		wantLines(t, actions(p), []string{"update User/alice spec.identity,spec.password"})
		u, _ := users.ByUsername(ctx, "alice")
		if u.PasswordHash != "" || !u.Identity.IsZero() {
			t.Errorf("spec %s did not clear: %+v", clear, u)
		}
	}
}

func TestUserSpecReportsAnEnvPassword(t *testing.T) {
	docs := newEnv(t).parse(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: a}\nspec: {password: {valueFrom: {env: P}}}\n"+
		"---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: b}\nspec: {password: {hash: \""+aliceHash+"\"}}\n")
	if !docs[0].User.UsesEnv() || docs[1].User.UsesEnv() {
		t.Errorf("UsesEnv = %v, %v", docs[0].User.UsesEnv(), docs[1].User.UsesEnv())
	}
}
