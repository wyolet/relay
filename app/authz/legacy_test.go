// legacy_test.go keeps the evaluator app/authz ran before it delegated to auth/rbac, verbatim apart from being test-local, so equivalence_test.go can require the two to agree.

package authz_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/user"
)

var legacyCatalogKinds = map[string]bool{
	"providers": true, "hosts": true, "models": true,
	"host-bindings": true, "pricings": true, "rate-limits": true,
}

var legacySharedCatalogKinds = map[string]bool{
	"providers": true, "hosts": true, "models": true,
	"host-bindings": true, "pricings": true,
}

var legacyScopedKinds = map[string]bool{
	"keys": true, "policies": true, "host-keys": true, "service-accounts": true,
	"policy-bindings": true, "role-bindings": true, "projects": true,
	"teams": true, "groups": true,
}

var legacyPlurals = map[string]string{
	"provider":        "providers",
	"host":            "hosts",
	"model":           "models",
	"host-binding":    "host-bindings",
	"pricing":         "pricings",
	"rate-limit":      "rate-limits",
	"policy":          "policies",
	"host-key":        "host-keys",
	"key":             "keys",
	"team":            "teams",
	"project":         "projects",
	"group":           "groups",
	"role":            "roles",
	"role-binding":    "role-bindings",
	"policy-binding":  "policy-bindings",
	"service-account": "service-accounts",
	"user":            "users",
	"token":           "tokens",
}

func legacyPlural(kind string) string {
	if p, ok := legacyPlurals[kind]; ok {
		return p
	}
	return kind
}

type legacyRBAC struct{ Snap func() authz.Snapshot }

func (r legacyRBAC) Authorize(ctx context.Context, action string, res authz.Resource) error {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return authz.ErrUnauthenticated
	}
	if legacyAdminActor(a) {
		return nil
	}
	kind, verb := legacySplitAction(action)
	if res.Owner != nil && legacyOwnedBy(*res.Owner, a) && !legacySharedCatalogKinds[kind] {
		return nil
	}
	if (verb == "get" || verb == "list") && legacyCatalogKinds[kind] &&
		(res.Owner == nil || legacyIsCatalogOwner(*res.Owner)) {
		return nil
	}
	if (verb == "get" || verb == "list") && kind == "policies" && res.Owner != nil &&
		(res.Owner.Kind == meta.OwnerSystem || res.Owner.Kind == meta.OwnerHost) {
		return nil
	}
	if (verb == "get" || verb == "list") && kind == "roles" &&
		res.Owner != nil && res.Owner.Kind == meta.OwnerSystem {
		return nil
	}
	if verb == "list" && res.Owner == nil && legacyScopedKinds[kind] {
		return nil
	}
	if r.Snap == nil {
		return authz.ErrForbidden
	}
	snap := r.Snap()
	if snap == nil {
		return authz.ErrForbidden
	}
	if (verb == "get" || verb == "list") && kind == "teams" && res.ID != "" &&
		legacyBoundInTeamProject(snap, a.Subjects, res.ID) {
		return nil
	}
	chain := snap.ScopeChainFor(res.Kind, res.ID, res.Owner)
	anyScope := verb == "list" && res.Owner == nil
	for _, subj := range a.Subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if !anyScope && !legacyInChain(chain, b.Spec.Scope) {
				continue
			}
			if role, ok := snap.Role(b.Spec.RoleID); ok && legacyAllows(role, kind, verb) {
				return nil
			}
		}
	}
	return authz.ErrForbidden
}

// legacyAllows is role.Role.Allows as it was, so the comparison does not share the matcher under test.
func legacyAllows(r *role.Role, kind, verb string) bool {
	for _, rule := range r.Spec.Rules {
		if legacyCovers(rule.Kinds, kind) && legacyCovers(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func legacyCovers(set []string, want string) bool {
	for _, v := range set {
		if v == role.Wildcard || v == want {
			return true
		}
	}
	return false
}

func (r legacyRBAC) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	return r.Authorize(ctx, legacyPlural(kind)+".get", authz.Resource{Kind: kind, ID: id, Owner: &owner}) == nil
}

func legacyBoundInTeamProject(snap authz.Snapshot, subjects []string, teamID string) bool {
	projects := snap.ProjectsInTeam(teamID)
	if len(projects) == 0 {
		return false
	}
	for _, subj := range subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if b.Spec.Scope.Kind != meta.OwnerProject {
				continue
			}
			for _, p := range projects {
				if p.Meta.ID == b.Spec.Scope.ID {
					return true
				}
			}
		}
	}
	return false
}

func legacyInChain(chain []meta.Owner, scope meta.Owner) bool {
	for _, o := range chain {
		if o.Kind == scope.Kind && o.ID == scope.ID {
			return true
		}
	}
	return false
}

func legacyOwnedBy(o meta.Owner, a *actor.Actor) bool {
	return o.Kind == meta.OwnerUser && o.ID != "" && o.ID == a.UserID
}

func legacyAdminActor(a *actor.Actor) bool {
	return a.AdminToken || a.HasRole(user.RoleAdmin)
}

func legacyIsAdmin(ctx context.Context) bool {
	a := actor.From(ctx)
	return a != nil && a.IsAuthenticated() && legacyAdminActor(a)
}

func legacyIsCatalogOwner(o meta.Owner) bool {
	switch o.Kind {
	case meta.OwnerSystem, meta.OwnerProvider, meta.OwnerHost:
		return true
	}
	return false
}

func legacySplitAction(action string) (kind, verb string) {
	kind, verb = action, action
	if i := strings.IndexByte(action, '.'); i >= 0 {
		kind = action[:i]
	}
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		verb = action[i+1:]
	}
	return kind, verb
}

func legacyCheckGrant(ctx context.Context, a authz.Authorizer, r *role.Role, scope meta.Owner) error {
	if a == nil || r == nil || legacyIsAdmin(ctx) {
		return nil
	}
	owner := scope
	for _, rule := range r.Spec.Rules {
		for _, kind := range legacyExpand(rule.Kinds, role.Kinds) {
			for _, verb := range legacyExpand(rule.Verbs, role.Verbs) {
				res := authz.Resource{Kind: authz.Singular(kind), Owner: &owner}
				if err := a.Authorize(ctx, kind+"."+verb, res); err != nil {
					return fmt.Errorf("%w: binding role %q would grant %s.%s, which you do not hold at this scope",
						authz.ErrForbidden, r.Meta.Name, kind, verb)
				}
			}
		}
	}
	return nil
}

func legacyExpand(set []string, all []string) []string {
	for _, v := range set {
		if v != role.Wildcard {
			continue
		}
		out := make([]string, 0, len(all))
		for _, a := range all {
			if a != role.Wildcard {
				out = append(out, a)
			}
		}
		return out
	}
	return set
}
