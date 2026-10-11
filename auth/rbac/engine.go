package rbac

import (
	"context"
	"errors"
	"slices"

	"github.com/wyolet/relay/auth/authz"
)

// Source is where the engine reads bindings and roles. A lookup error fails the decision with that error; it is never read as a denial. The engine is done with what a method returns before calling that method again, so a Source may reuse its buffers.
type Source interface {
	// BindingsForSubject returns the bindings naming subject.
	BindingsForSubject(ctx context.Context, subject string) ([]Binding, error)
	// Role returns the role with this id, or false when there is none.
	Role(ctx context.Context, id string) (Role, bool, error)
}

// ScopeResolver places a resource in its scopes.
type ScopeResolver interface {
	// ChainFor returns the scopes res lives in, most specific first, ending in the engine's global scope. A resource may be its own innermost scope, so a binding can attach to one resource. CheckGrant asks with Resource{Owner: &scope} and no ID, so a resolver must read an owner equal to a scope as living in that scope.
	ChainFor(ctx context.Context, res authz.Resource) ([]Scope, error)
}

// Decision is a product rule's answer.
type Decision int

const (
	// Continue leaves the request to the next rule and then the bindings.
	Continue Decision = iota
	// Allow admits the request, subject to the credential scope.
	Allow
	// Deny refuses the request; no later rule or binding is consulted.
	Deny
)

// ProductRule is an ownership-style shortcut a product consults ahead of the bindings. Grants that a role could express belong in a Source instead, so they go through Visible, ScopeOf and the grant check like any binding.
type ProductRule interface {
	Decide(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) (Decision, error)
}

// RuleFunc adapts a function to ProductRule. It has no Filter, so an engine holding one cannot answer ScopeOf.
type RuleFunc func(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) (Decision, error)

// Decide calls f.
func (f RuleFunc) Decide(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) (Decision, error) {
	return f(ctx, p, kind, verb, res)
}

// Engine evaluates decisions. It holds no state of its own, so a product may build one per call or share one.
type Engine struct {
	Source Source
	Scopes ScopeResolver
	// Rules run in order before the bindings; the first Allow or Deny wins.
	Rules []ProductRule
	// Global is the scope every chain ends in. ScopeOf reads a binding there as reaching every resource.
	Global Scope
}

// Authorize returns nil when p may perform action on res, authz.ErrUnauthenticated or authz.ErrForbidden when not, and any other error when a lookup failed.
func (e *Engine) Authorize(ctx context.Context, p *Principal, action string, res authz.Resource) error {
	kind, verb := SplitAction(action)
	return e.decide(ctx, p, kind, verb, res)
}

// Allowed is Authorize as a boolean; the error is only a failed lookup.
func (e *Engine) Allowed(ctx context.Context, p *Principal, action string, res authz.Resource) (bool, error) {
	err := e.Authorize(ctx, p, action, res)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrUnauthenticated) {
		return false, nil
	}
	return false, err
}

// Visible reports whether p may get res, an action of kind. A failed lookup hides the resource.
func (e *Engine) Visible(ctx context.Context, p *Principal, kind string, res authz.Resource) bool {
	return e.decide(ctx, p, kind, VerbGet, res) == nil
}

func (e *Engine) decide(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) error {
	if !p.Authenticated() {
		return authz.ErrUnauthenticated
	}
	ok, chain, err := e.allows(ctx, p, kind, verb, res)
	if err != nil {
		return err
	}
	if ok && p.Credential != nil {
		ok, err = e.credentialPermits(ctx, p.Credential, kind, verb, res, chain)
		if err != nil {
			return err
		}
	}
	if !ok {
		return authz.ErrForbidden
	}
	return nil
}

// allows also returns res's scope chain when the bindings had to resolve it, so the credential check can reuse it.
func (e *Engine) allows(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) (bool, []Scope, error) {
	if p.Admin {
		return true, nil, nil
	}
	for _, r := range e.Rules {
		d, err := r.Decide(ctx, p, kind, verb, res)
		if err != nil {
			return false, nil, err
		}
		switch d {
		case Allow:
			return true, nil, nil
		case Deny:
			return false, nil, nil
		}
	}
	return e.bound(ctx, p, kind, verb, res)
}

// bound scans the bindings of p's subjects for one at a scope in res's chain whose role covers (kind, verb).
func (e *Engine) bound(ctx context.Context, p *Principal, kind, verb string, res authz.Resource) (bool, []Scope, error) {
	if e.Source == nil {
		return false, nil, nil
	}
	// The any-scope path: see VerbList for why an unowned list needs no chain.
	anyScope := verb == VerbList && res.Owner == nil
	var chain []Scope
	if !anyScope {
		if e.Scopes == nil {
			return false, nil, nil
		}
		var err error
		if chain, err = e.Scopes.ChainFor(ctx, res); err != nil {
			return false, nil, err
		}
	}
	for i := range subjectCount(p) {
		bindings, err := e.Source.BindingsForSubject(ctx, subjectAt(p, i))
		if err != nil {
			return false, nil, err
		}
		for _, b := range bindings {
			if !anyScope && !slices.Contains(chain, b.Scope) {
				continue
			}
			role, found, err := e.Source.Role(ctx, b.RoleID)
			if err != nil {
				return false, nil, err
			}
			if found && role.Allows(kind, verb) {
				return true, chain, nil
			}
		}
	}
	return false, chain, nil
}

// credentialPermits applies c to an allowed request. chain is res's scope chain when already resolved, else nil.
func (e *Engine) credentialPermits(ctx context.Context, c *CredentialScope, kind, verb string, res authz.Resource, chain []Scope) (bool, error) {
	if !rulesAllow(c.Rules, kind, verb) {
		return false, nil
	}
	within, limited := c.Within[kind]
	if !limited {
		return true, nil
	}
	if len(within) == 0 {
		return false, nil
	}
	if res.ID == "" && res.Owner == nil {
		return verb == VerbList, nil
	}
	if chain == nil {
		if e.Scopes == nil {
			return false, nil
		}
		var err error
		if chain, err = e.Scopes.ChainFor(ctx, res); err != nil {
			return false, err
		}
	}
	for _, s := range within {
		if slices.Contains(chain, s) {
			return true, nil
		}
	}
	return false, nil
}

// subjectCount and subjectAt walk p's subjects followed by SubjectAuthenticated, unless p already lists it.
func subjectCount(p *Principal) int {
	if slices.Contains(p.Subjects, SubjectAuthenticated) {
		return len(p.Subjects)
	}
	return len(p.Subjects) + 1
}

func subjectAt(p *Principal, i int) string {
	if i < len(p.Subjects) {
		return p.Subjects[i]
	}
	return SubjectAuthenticated
}
