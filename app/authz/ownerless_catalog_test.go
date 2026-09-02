package authz_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
)

// A catalog row shipped before owners carried an id lands with
// owner {kind: user, id: ""}. TestOwnerlessUserOwnedCatalogRowIsReadable pins
// that it reads like the catalog row it is, instead of being a personal row
// belonging to nobody — while a row owned by a real user stays private.
func TestOwnerlessUserOwnedCatalogRowIsReadable(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	ownerless := &meta.Owner{Kind: meta.OwnerUser}

	bob := actorOf(bobID, subjectsOf(bobID))
	if err := rbac.Authorize(ctxOf(bob), "models.get", authz.Resource{Kind: "model", Owner: ownerless}); err != nil {
		t.Fatalf("read of an owner-less catalog row = %v, want allowed", err)
	}
	if err := rbac.Authorize(ctxOf(bob), "models.update", authz.Resource{Kind: "model", Owner: ownerless}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("write to an owner-less catalog row = %v, want forbidden", err)
	}
	if err := rbac.Authorize(ctxOf(bob), "models.get", authz.Resource{Kind: "model", Owner: userOwner(aliceID)}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("read of another user's row = %v, want forbidden", err)
	}
}
