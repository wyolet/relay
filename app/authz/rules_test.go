package authz_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
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

// Each shortcut ahead of the role bindings admits exactly its own shape of
// call: the decision names the rule, and the neighbouring call it must not
// admit falls through to a denial.
func TestProductRules(t *testing.T) {
	cat := newFixture(t)
	r := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	strangerID := ids.New()
	stranger := actorOf(strangerID, subjectsOf(strangerID))
	frank := actorOf(frankID, subjectsOf(frankID, "ml")) // project-admin on P2 (team T1)
	gina := actorOf(ginaID, subjectsOf(ginaID))          // bound at the disabled P4 (team T2)
	host := &meta.Owner{Kind: meta.OwnerHost, ID: ids.New()}
	team := &meta.Owner{Kind: meta.OwnerTeam, ID: t1ID}
	res := func(kind, id string, o *meta.Owner) authz.Resource {
		return authz.Resource{Kind: kind, ID: id, Owner: o}
	}

	tests := []struct {
		name   string
		who    *actor.Actor
		action string
		res    authz.Resource
		rule   string // "" = no rule admits it; the stranger holds no binding, so it is denied
	}{
		{"own row, every verb", stranger, "host-keys.delete", res("host-key", "", userOwner(strangerID)), "personal-row"},
		{"own row, create", stranger, "policies.create", res("policy", "", userOwner(strangerID)), "personal-row"},
		{"own row of a non-catalog kind", stranger, "rate-limits.update", res("rate-limit", "", userOwner(strangerID)), "personal-row"},
		{"someone else's row", stranger, "host-keys.get", res("host-key", "", userOwner(aliceID)), ""},
		{"user owner without an id", stranger, "host-keys.get", res("host-key", "", &meta.Owner{Kind: meta.OwnerUser}), ""},
		{"own row of a shared catalog kind", stranger, "models.update", res("model", "", userOwner(strangerID)), ""},
		{"own row of a shared catalog kind, read", stranger, "hosts.get", res("host", "", userOwner(strangerID)), ""},

		{"catalog get", stranger, "models.get", res("model", "m", providerOwner()), "catalog-read"},
		{"catalog list without an owner", stranger, "pricings.list", res("pricing", "", nil), "catalog-read"},
		{"catalog get, host-owned", stranger, "host-bindings.get", res("host-binding", "b", host), "catalog-read"},
		{"catalog get, system-owned rate limit", stranger, "rate-limits.get", res("rate-limit", "r", &globalScope), "catalog-read"},
		{"catalog write", stranger, "models.update", res("model", "m", providerOwner()), ""},
		{"catalog kind owned by a tenant", stranger, "rate-limits.get", res("rate-limit", "r", team), ""},

		{"system policy", stranger, "policies.get", res("policy", "p", &globalScope), "shared-policy-read"},
		{"host policy", stranger, "policies.list", res("policy", "p", host), "shared-policy-read"},
		{"shared policy write", stranger, "policies.update", res("policy", "p", host), ""},
		{"project policy", stranger, "policies.get", res("policy", "p", projectOwner(p1ID)), ""},

		{"system role get", stranger, "roles.get", res("role", "r", &globalScope), "system-role-read"},
		{"system role list", stranger, "roles.list", res("role", "r", &globalScope), "system-role-read"},
		{"system role write", stranger, "roles.update", res("role", "r", &globalScope), ""},
		{"team role", stranger, "roles.get", res("role", "r", team), ""},
		{"role list without an owner", stranger, "roles.list", res("role", "", nil), ""},

		{"scoped list", stranger, "keys.list", res("key", "", nil), "scoped-list"},
		{"scoped list naming an owner", stranger, "keys.list", res("key", "", projectOwner(p1ID)), ""},
		{"scoped get without an owner", stranger, "keys.get", res("key", "", nil), ""},
		{"users are not a scoped list", stranger, "users.list", res("user", "", nil), ""},
		{"usage is not a scoped list", stranger, "usage.list", res("usage", "", nil), ""},

		{"parent team get", frank, "teams.get", res("team", t1ID, &globalScope), "parent-team-read"},
		{"parent team list", frank, "teams.list", res("team", t1ID, &globalScope), "parent-team-read"},
		{"parent team write", frank, "teams.update", res("team", t1ID, &globalScope), ""},
		{"unrelated team", frank, "teams.get", res("team", t2ID, &globalScope), ""},
		{"team without an id", frank, "teams.get", res("team", "", &globalScope), ""},
		{"parent of a disabled project", gina, "teams.get", res("team", t2ID, &globalScope), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, err := r.Explain(t.Context(), tt.who, tt.action, tt.res)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			wantReason := rbac.ReasonNone
			if tt.rule != "" {
				wantReason = rbac.ReasonRule
			}
			if x.Rule != tt.rule || x.Reason != wantReason || x.Allowed != (tt.rule != "") {
				t.Fatalf("Explain = %+v, want rule %q", x, tt.rule)
			}
			var want error
			if tt.rule == "" {
				want = authz.ErrForbidden
			}
			if got := r.Authorize(ctxOf(tt.who), tt.action, tt.res); got != want {
				t.Fatalf("Authorize = %v, want %v", got, want)
			}
		})
	}

	for kind := range slices.Values([]string{"keys", "policies", "host-keys", "service-accounts", "policy-bindings",
		"role-bindings", "projects", "teams", "groups", "accounts", "pools", "budgets"}) {
		if err := r.Authorize(ctxOf(stranger), kind+".list", authz.Resource{Kind: authz.Singular(kind)}); err != nil {
			t.Errorf("%s.list = %v, want the scoped list open to a caller with no binding", kind, err)
		}
	}
}

// The shortcuts that read no snapshot still decide without one.
func TestProductRulesWithoutASnapshot(t *testing.T) {
	r := authz.RBAC{}
	alice := actorOf(aliceID, subjectsOf(aliceID))
	for action, res := range map[string]authz.Resource{
		"roles.get":    {Kind: "role", ID: "r-1", Owner: &globalScope},
		"keys.list":    {Kind: "key"},
		"models.get":   {Kind: "model", Owner: providerOwner()},
		"keys.delete":  {Kind: "key", Owner: userOwner(aliceID)},
		"policies.get": {Kind: "policy", Owner: &globalScope},
	} {
		if err := r.Authorize(ctxOf(alice), action, res); err != nil {
			t.Errorf("%s = %v, want allowed", action, err)
		}
	}
	if err := r.Authorize(ctxOf(alice), "teams.get", authz.Resource{Kind: "team", ID: t1ID, Owner: &globalScope}); err != authz.ErrForbidden {
		t.Errorf("teams.get = %v, want forbidden", err)
	}
}

func TestKindNames(t *testing.T) {
	for singular, plural := range map[string]string{
		"provider": "providers", "host": "hosts", "model": "models", "host-binding": "host-bindings",
		"pricing": "pricings", "rate-limit": "rate-limits", "policy": "policies", "host-key": "host-keys",
		"account": "accounts", "pool": "pools", "budget": "budgets", "key": "keys", "team": "teams",
		"project": "projects", "group": "groups", "role": "roles", "role-binding": "role-bindings",
		"policy-binding": "policy-bindings", "service-account": "service-accounts", "user": "users",
		"token": "tokens", "scim-client": "scim-clients", "secret-key": "secret-keys",
		// Kinds with no singular form name themselves both ways.
		"usage": "usage", "settings": "settings", "logs": "logs",
	} {
		if got := authz.Plural(singular); got != plural {
			t.Errorf("Plural(%q) = %q, want %q", singular, got, plural)
		}
		if got := authz.Singular(plural); got != singular {
			t.Errorf("Singular(%q) = %q, want %q", plural, got, singular)
		}
	}
	// Every kind a role rule may name resolves to a singular that maps back.
	for _, kind := range role.Kinds {
		if kind != role.Wildcard && authz.Plural(authz.Singular(kind)) != kind {
			t.Errorf("role kind %q does not round-trip: singular %q", kind, authz.Singular(kind))
		}
	}

	shared := map[string]bool{"host": true, "host-binding": true, "provider": true, "model": true, "pricing": true}
	for _, kind := range role.Kinds {
		k := authz.Singular(kind)
		if got := authz.SharedCatalogKind(k); got != shared[k] {
			t.Errorf("SharedCatalogKind(%q) = %v, want %v", k, got, shared[k])
		}
	}

	organization := map[string]bool{"policy": true, "rate-limit": true, "budget": true, "pool": true}
	for _, kind := range role.Kinds {
		k := authz.Singular(kind)
		if got := authz.OrganizationKind(k); got != organization[k] {
			t.Errorf("OrganizationKind(%q) = %v, want %v", k, got, organization[k])
		}
	}
}

// An organization row is an admin's alone: no binding reaches it, not even a global one, and ownership grants nothing.
func TestOrganizationRows(t *testing.T) {
	cat := newFixture(t)
	r := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	org := &meta.Owner{Kind: meta.OwnerOrganization}
	strangerID := ids.New()
	globalEditor := actorOf(daveID, subjectsOf(daveID))
	teamAdmin := actorOf(aliceID, subjectsOf(aliceID))
	refused := map[string]*actor.Actor{
		"global catalog editor": globalEditor,
		"team admin":            teamAdmin,
		"no binding":            actorOf(strangerID, subjectsOf(strangerID)),
	}
	admitted := map[string]*actor.Actor{
		"admin role":  actorOf(ids.New(), nil, user.RoleAdmin),
		"admin token": {AdminToken: true},
	}
	for _, kind := range []string{"rate-limits", "policies", "budgets", "pools"} {
		for _, verb := range []string{"get", "list", "create", "update", "delete"} {
			res := authz.Resource{Kind: authz.Singular(kind), ID: ids.New(), Owner: org}
			for name, a := range refused {
				x, err := r.Explain(t.Context(), a, kind+"."+verb, res)
				if err != nil || x.Allowed || x.Rule != "organization-row" {
					t.Errorf("%s: %s.%s explained %+v (%v), want refused by organization-row", name, kind, verb, x, err)
				}
				if err := r.Authorize(ctxOf(a), kind+"."+verb, res); err != authz.ErrForbidden {
					t.Errorf("%s: %s.%s = %v, want forbidden", name, kind, verb, err)
				}
			}
			for name, a := range admitted {
				if err := r.Authorize(ctxOf(a), kind+"."+verb, res); err != nil {
					t.Errorf("%s: %s.%s = %v, want allowed", name, kind, verb, err)
				}
			}
		}
	}
	if err := r.Authorize(ctxOf(globalEditor), "rate-limits.update", authz.Resource{Kind: "rate-limit", ID: ids.New(), Owner: &globalScope}); err != nil {
		t.Fatalf("global editor on a system rate limit = %v, want allowed", err)
	}

	// A filter cannot take back what a global grant selects, so ScopeOf declines there and answers everywhere else.
	if _, err := authz.ScopeOf(ctxOf(globalEditor), r, "get", "rate-limits"); !errors.Is(err, rbac.ErrUnfilterable) {
		t.Fatalf("ScopeOf for a global grant on rate-limits = %v, want ErrUnfilterable", err)
	}
	if _, err := authz.ScopeOf(ctxOf(globalEditor), r, "get", "models"); err != nil {
		t.Fatalf("ScopeOf on a kind with no organization rows = %v", err)
	}
	if _, err := authz.ScopeOf(ctxOf(teamAdmin), r, "get", "rate-limits"); err != nil {
		t.Fatalf("ScopeOf without a global grant = %v", err)
	}
}
