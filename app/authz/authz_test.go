package authz_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/authz"
)

// Single-user mode: authenticated is the whole check, and unauthenticated
// is still refused.
func TestAlwaysAllowAuthenticated(t *testing.T) {
	a := authz.AlwaysAllowAuthenticated{}
	if err := a.Authorize(ctxOf(nil), "keys.delete", authz.Resource{Kind: "key"}); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("unauthenticated = %v, want unauthenticated", err)
	}
	if err := a.Authorize(ctxOf(actorOf(bobID, nil)), "keys.delete",
		authz.Resource{Kind: "key", Owner: userOwner(aliceID)}); err != nil {
		t.Fatalf("authenticated = %v, want allowed", err)
	}
}
