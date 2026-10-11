// Binding a role grants every permission in it, so the binder must already
// hold each one at the binding's scope.

package authz

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/auth/rbac"
)

// CheckGrant reports whether the caller may bind r at scope. Every (kind,
// verb) the role's rules cover is authorized against the caller with the
// binding's scope as the resource owner; the first one the caller does not
// hold refuses the binding. A wildcard rule expands to the full closed
// vocabulary, so binding a wildcard role requires holding everything.
// Admins are exempt, and so is a nil authorizer (a loader running as the
// deployment itself).
//
// It goes through a rather than the engine's own grant check, so a wrapping authorizer (audit) and single-user mode see every probe.
func CheckGrant(ctx context.Context, a Authorizer, r *role.Role, scope meta.Owner) error {
	if a == nil || r == nil || IsAdmin(ctx) {
		return nil
	}
	owner := scope
	rules := make([]rbac.Rule, len(r.Spec.Rules))
	for i, rule := range r.Spec.Rules {
		rules[i] = rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
	}
	return rbac.EachPermission(rules, role.Kinds, role.Verbs, func(kind, verb string) error {
		res := Resource{Kind: Singular(kind), Owner: &owner}
		if err := a.Authorize(ctx, kind+"."+verb, res); err != nil {
			return fmt.Errorf("%w: binding role %q would grant %s.%s, which you do not hold at this scope",
				ErrForbidden, r.Meta.Name, kind, verb)
		}
		return nil
	})
}
