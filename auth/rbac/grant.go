package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/authz"
)

// CheckGrant reports whether p may bind role at scope: binding hands out every permission in the role, so p must already hold each one there. A wildcard in a rule expands over kinds or verbs, the product's full vocabulary; with a nil vocabulary it is checked as itself, so binding a wildcard takes holding one.
func (e *Engine) CheckGrant(ctx context.Context, p *Principal, role Role, scope Scope, kinds, verbs []string) error {
	owner := authz.Owner(scope)
	return EachPermission(role.Rules, kinds, verbs, func(kind, verb string) error {
		err := e.decide(ctx, p, kind, verb, authz.Resource{Kind: kind, Owner: &owner})
		if errors.Is(err, authz.ErrForbidden) {
			return fmt.Errorf("%w: role %q grants %s.%s, which the caller does not hold at this scope",
				authz.ErrForbidden, role.ID, kind, verb)
		}
		return err
	})
}

// EachPermission calls fn for every (kind, verb) the rules cover, in rule order, expanding a wildcard over kinds or verbs as CheckGrant does, and stops at the first error.
func EachPermission(rules []Rule, kinds, verbs []string, fn func(kind, verb string) error) error {
	for _, r := range rules {
		for _, kind := range expand(r.Kinds, kinds) {
			for _, verb := range expand(r.Verbs, verbs) {
				if err := fn(kind, verb); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// expand resolves a rule's list against the vocabulary: a wildcard stands for every entry of it except the wildcard itself.
func expand(set, vocabulary []string) []string {
	if vocabulary == nil {
		return set
	}
	for _, v := range set {
		if v != Wildcard {
			continue
		}
		out := make([]string, 0, len(vocabulary))
		for _, a := range vocabulary {
			if a != Wildcard {
				out = append(out, a)
			}
		}
		return out
	}
	return set
}
