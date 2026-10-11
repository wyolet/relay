package rbac

import (
	"context"
	"errors"
	"slices"

	"github.com/wyolet/relay/auth/authz"
)

// ErrUnfilterable is returned by ScopeOf when a product rule cannot describe what it allows as a Filter; the caller filters row by row with Visible.
var ErrUnfilterable = errors.New("rbac: rule cannot be expressed as a filter")

// Filter is ScopeOf's answer: which owned resources of a kind the caller may act on, in a form a store can turn into a query. A resource passes when All is set, its scope chain holds one of Scopes, its owner is one of Owners, or its owner's kind is one of OwnerKinds; and, when Within is not empty, its scope chain also holds one of Within. A scope in Scopes or Within whose kind is the queried kind means "this id": the resource is its own innermost scope.
type Filter struct {
	All        bool
	Scopes     []Scope
	Owners     []authz.Owner
	OwnerKinds []string
	Within     []Scope
}

// None reports whether no resource passes.
func (f Filter) None() bool {
	return !f.All && len(f.Scopes) == 0 && len(f.Owners) == 0 && len(f.OwnerKinds) == 0
}

// FilterRule is a ProductRule that can also describe, for ScopeOf, every resource of a kind it allows verb on. A rule that may Deny such a resource returns ErrUnfilterable: a Filter only adds.
type FilterRule interface {
	ProductRule
	Filter(ctx context.Context, p *Principal, kind, verb string) (Filter, error)
}

// ScopeOf returns the owned resources of kind that p may perform verb on, as a Filter for a store-side query; Visible stays the per-row check, and the two agree on every owned resource as long as each rule's Filter describes its Decide. A resource with no owner (a list call) is outside what ScopeOf describes. Every product rule must be a FilterRule, or ScopeOf returns ErrUnfilterable.
func (e *Engine) ScopeOf(ctx context.Context, p *Principal, verb, kind string) (Filter, error) {
	if !p.Authenticated() {
		return Filter{}, authz.ErrUnauthenticated
	}
	f, err := e.grantedFilter(ctx, p, kind, verb)
	if err != nil || f.None() || p.Credential == nil {
		return f, err
	}
	if !rulesAllow(p.Credential.Rules, kind, verb) {
		return Filter{}, nil
	}
	if within, limited := p.Credential.Within[kind]; limited {
		if len(within) == 0 {
			return Filter{}, nil
		}
		f.Within = slices.Clone(within)
	}
	return f, nil
}

func (e *Engine) grantedFilter(ctx context.Context, p *Principal, kind, verb string) (Filter, error) {
	if p.Admin {
		return Filter{All: true}, nil
	}
	var f Filter
	for _, r := range e.Rules {
		fr, ok := r.(FilterRule)
		if !ok {
			return Filter{}, ErrUnfilterable
		}
		rf, err := fr.Filter(ctx, p, kind, verb)
		if err != nil {
			return Filter{}, err
		}
		// A union of filters cannot carry one rule's own limit.
		if len(rf.Within) > 0 {
			return Filter{}, ErrUnfilterable
		}
		if rf.All {
			return Filter{All: true}, nil
		}
		f.Scopes = appendNew(f.Scopes, rf.Scopes...)
		f.Owners = appendNew(f.Owners, rf.Owners...)
		f.OwnerKinds = appendNew(f.OwnerKinds, rf.OwnerKinds...)
	}
	if e.Source == nil {
		return f, nil
	}
	for i := range subjectCount(p) {
		bindings, err := e.Source.BindingsForSubject(ctx, subjectAt(p, i))
		if err != nil {
			return Filter{}, err
		}
		for _, b := range bindings {
			if slices.Contains(f.Scopes, b.Scope) {
				continue
			}
			role, found, err := e.Source.Role(ctx, b.RoleID)
			if err != nil {
				return Filter{}, err
			}
			if !found || !role.Allows(kind, verb) {
				continue
			}
			if b.Scope == e.Global {
				return Filter{All: true}, nil
			}
			f.Scopes = append(f.Scopes, b.Scope)
		}
	}
	return f, nil
}

func appendNew[T comparable](dst []T, src ...T) []T {
	for _, v := range src {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
