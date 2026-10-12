package directory_test

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/password"
)

// Every change to how an account signs in, through every directory path, ends its sessions and bumps its token version: a session or token minted under the old credentials must not outlive them.
func TestCredentialChangesEndSessionsAndTokens(t *testing.T) {
	h := newHarness(t)
	hash, err := password.Hash("s3cret")
	must(t, err)
	other, err := password.Hash("other")
	must(t, err)
	idp := directory.Identity{Issuer: "https://idp.example.com", Subject: "ada"}
	u := &directory.User{Username: "ada", PasswordHash: hash, Identity: idp}
	must(t, h.d.CreateUser(ctx, admin, u))
	extra := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "ada-2"}}
	update := func(edit func(*directory.User)) func() error {
		return func() error {
			cur, _ := h.s.Users.Get(ctx, u.ID)
			edit(cur)
			return h.d.UpdateUser(ctx, admin, cur)
		}
	}

	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"password changed", update(func(x *directory.User) { x.PasswordHash = other })},
		{"password cleared", update(func(x *directory.User) { x.PasswordHash = "" })},
		{"password set", update(func(x *directory.User) { x.PasswordHash = hash })},
		{"primary identity changed", update(func(x *directory.User) { x.Identity.Subject = "ada-1" })},
		{"identity added", func() error { return h.d.AddIdentity(ctx, u.ID, extra) }},
		{"identity removed", func() error { return h.d.RemoveIdentity(ctx, u.ID, extra.Identity) }},
		{"identities cleared", func() error { return h.d.RemoveIdentities(ctx, u.ID) }},
		{"first identity linked", func() error { return h.d.AddFirstIdentity(ctx, u.ID, idp) }},
	} {
		before, _ := h.s.Users.Get(ctx, u.ID)
		h.rev.ended = nil
		must(t, step.write())
		after, _ := h.s.Users.Get(ctx, u.ID)
		if !slices.Equal(h.rev.ended, []string{u.ID}) {
			t.Fatalf("%s: sessions ended for %v, want the user's", step.name, h.rev.ended)
		}
		if after.TokenVersion != before.TokenVersion+1 {
			t.Fatalf("%s: token version %d -> %d, want one bump", step.name, before.TokenVersion, after.TokenVersion)
		}
	}

	// An edit that leaves the credentials alone keeps both.
	before, _ := h.s.Users.Get(ctx, u.ID)
	h.rev.ended = nil
	must(t, update(func(x *directory.User) { x.Email = "ada@example.com" })())
	if after, _ := h.s.Users.Get(ctx, u.ID); len(h.rev.ended) != 0 || after.TokenVersion != before.TokenVersion {
		t.Fatalf("an email edit ended %v and moved the token version %d -> %d", h.rev.ended, before.TokenVersion, after.TokenVersion)
	}
}
