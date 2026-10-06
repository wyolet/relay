package authz_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

var sharedCatalogPlurals = []string{"hosts", "host-bindings", "providers", "models", "pricings"}

// Naming yourself owner of a shared catalog row grants nothing: a signed-in
// user with no binding, and a project developer, are refused every write on
// it — including a row that already carries their own user owner.
func TestSharedCatalogKindsIgnoreOwnership(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	unbound := ids.New()
	callers := map[string]*actor.Actor{
		"no role binding":   actorOf(unbound, subjectsOf(unbound)),
		"project developer": actorOf(bobID, subjectsOf(bobID)),
	}
	for name, a := range callers {
		for _, kind := range sharedCatalogPlurals {
			owners := []*meta.Owner{userOwner(a.UserID), {Kind: meta.OwnerUser}, projectOwner(p1ID), {Kind: meta.OwnerTeam, ID: t1ID}, nil, &globalScope}
			for _, verb := range []string{"create", "update", "delete"} {
				for _, o := range owners {
					res := authz.Resource{Kind: authz.Singular(kind), ID: ids.New(), Owner: o}
					if err := rbac.Authorize(ctxOf(a), kind+"."+verb, res); !errors.Is(err, authz.ErrForbidden) {
						t.Errorf("%s: %s.%s owned by %+v = %v, want forbidden", name, kind, verb, o, err)
					}
				}
			}
			// Reading a row they "own" is not granted by ownership either.
			if err := rbac.Authorize(ctxOf(a), kind+".get", authz.Resource{Kind: authz.Singular(kind), Owner: userOwner(a.UserID)}); !errors.Is(err, authz.ErrForbidden) {
				t.Errorf("%s: %s.get on a user-owned row = %v, want forbidden", name, kind, err)
			}
		}
	}
}

// Write access to shared catalog rows comes from the role matrix.
func TestSharedCatalogKindsWritableThroughRoles(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	callers := map[string]*actor.Actor{
		"catalog-editor": actorOf(daveID, subjectsOf(daveID)),
		"admin role":     actorOf(ids.New(), nil, user.RoleAdmin),
		"admin token":    {AdminToken: true},
	}
	for name, a := range callers {
		for _, kind := range sharedCatalogPlurals {
			for _, verb := range []string{"create", "update", "delete"} {
				for _, o := range []*meta.Owner{{}, &globalScope} {
					res := authz.Resource{Kind: authz.Singular(kind), Owner: o}
					if err := rbac.Authorize(ctxOf(a), kind+"."+verb, res); err != nil {
						t.Errorf("%s: %s.%s owned by %+v = %v, want allowed", name, kind, verb, o, err)
					}
				}
			}
		}
	}
}

// Kinds that legitimately hold personal rows keep the owner shortcut.
func TestPersonalRowsStillOwnerOperated(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	unbound := ids.New()
	a := actorOf(unbound, subjectsOf(unbound))
	for _, kind := range []string{"host-keys", "rate-limits", "policies", "keys"} {
		for _, verb := range []string{"create", "get", "update", "delete"} {
			if err := rbac.Authorize(ctxOf(a), kind+"."+verb, authz.Resource{Kind: authz.Singular(kind), Owner: userOwner(unbound)}); err != nil {
				t.Errorf("%s.%s on the caller's own row = %v, want allowed", kind, verb, err)
			}
		}
	}
}

func TestSharedCatalogKind(t *testing.T) {
	for _, k := range []string{"host", "host-binding", "provider", "model", "pricing"} {
		if !authz.SharedCatalogKind(k) {
			t.Errorf("SharedCatalogKind(%q) = false", k)
		}
	}
	for _, k := range []string{"host-key", "rate-limit", "policy", "key", "role", "team"} {
		if authz.SharedCatalogKind(k) {
			t.Errorf("SharedCatalogKind(%q) = true", k)
		}
	}
}
