package control

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/policy"
)

// A rotation must not strand the tokens minted before it: revoke-by-value
// verifies against the retired key as well as the live one.
func TestTokenRevokeByValuePreviousKey(t *testing.T) {
	f := newTokenFixture(t)
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	minted, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, "30m"))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	retired := f.signer.PublicKey()

	next := make([]byte, ed25519.SeedSize)
	for i := range next {
		next[i] = byte(200 - i)
	}
	f.signer.SetSeed(next)
	f.signer.SetPreviousPublicKey(retired)

	if _, err := revokeTokenByValue(ctx, f.deps, minted.Body.Token); err != nil {
		t.Fatalf("revokeTokenByValue after a rotation: %v", err)
	}
	f.denylist.mu.Lock()
	defer f.denylist.mu.Unlock()
	if _, ok := f.denylist.entries[policy.RevokedKey(f.team.Meta.ID, minted.Body.JTI)]; !ok {
		t.Fatalf("no denylist entry written; entries = %v", f.denylist.entries)
	}
}
