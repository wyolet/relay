// escalation.go holds the rule that keeps a RoleBinding from handing out
// more than the binder has: binding a role is granting every permission in
// it, so the binder must already hold each one at the binding's scope.
package authz

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
)

// CheckGrant reports whether the caller may bind r at scope. Every (kind,
// verb) the role's rules cover is authorized against the caller with the
// binding's scope as the resource owner; the first one the caller does not
// hold refuses the binding. A wildcard rule expands to the full closed
// vocabulary, so binding a wildcard role requires holding everything.
// Admins are exempt, and so is a nil authorizer (a loader running as the
// deployment itself).
func CheckGrant(ctx context.Context, a Authorizer, r *role.Role, scope meta.Owner) error {
	if a == nil || r == nil || IsAdmin(ctx) {
		return nil
	}
	owner := scope
	for _, rule := range r.Spec.Rules {
		for _, kind := range expand(rule.Kinds, role.Kinds) {
			for _, verb := range expand(rule.Verbs, role.Verbs) {
				res := Resource{Kind: Singular(kind), Owner: &owner}
				if err := a.Authorize(ctx, kind+"."+verb, res); err != nil {
					return fmt.Errorf("%w: binding role %q would grant %s.%s, which you do not hold at this scope",
						ErrForbidden, r.Meta.Name, kind, verb)
				}
			}
		}
	}
	return nil
}

// expand resolves a rule's list against the closed vocabulary: a wildcard
// stands for every entry in it (minus the wildcard itself).
func expand(set []string, all []string) []string {
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
