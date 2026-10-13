package rbac

import (
	"context"
	"errors"
	"slices"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/principal"
)

// Reason is what decided an Explanation.
type Reason string

const (
	// ReasonAdmin: the principal's Admin flag passed every rule and binding.
	ReasonAdmin Reason = "admin"
	// ReasonRule: a product rule allowed or denied the request.
	ReasonRule Reason = "rule"
	// ReasonBinding: a binding of one of the principal's subjects granted it.
	ReasonBinding Reason = "binding"
	// ReasonCredential: the principal held the permission, but its credential scope refused it.
	ReasonCredential Reason = "credential"
	// ReasonNone: nothing granted it.
	ReasonNone Reason = "none"
)

// Explanation is Explain's answer: the decision Authorize would make and what made it. Only the first match is reported; allows are additive, so one reason is enough.
type Explanation struct {
	Allowed bool
	Reason  Reason
	// Rule is the Name of the product rule that decided, when it has one.
	Rule string
	// Subject, RoleID and Scope are the binding that matched: the first binding the Source returned for Subject with that role and scope.
	Subject string
	RoleID  string
	Scope   Scope
	// Chain is the resource's scope chain, when the decision resolved one: an admin, a product rule and an unowned list decide without it.
	Chain []Scope
	// CredentialDenied is set with ReasonCredential; Rule or the binding fields still name what granted the permission the credential then refused.
	CredentialDenied bool
}

// NamedRule is a ProductRule that names itself in explanations.
type NamedRule interface {
	ProductRule
	Name() string
}

// Explain answers what Authorize would for the same arguments, and why. A denial is an Explanation with Allowed unset, not an error; the error is authz.ErrUnauthenticated or a failed lookup, exactly as Authorize returns them.
func (e *Engine) Explain(ctx context.Context, p *principal.Principal, action string, res authz.Resource) (Explanation, error) {
	kind, verb := SplitAction(action)
	x := Explanation{Reason: ReasonNone}
	err := e.decide(ctx, p, kind, verb, res, &x)
	if err != nil && !errors.Is(err, authz.ErrForbidden) {
		return Explanation{Reason: ReasonNone}, err
	}
	return x, nil
}

// The recorders below are called on every decision; a nil x is Authorize, which records nothing.

func (x *Explanation) decidedBy(r Reason) {
	if x != nil {
		x.Reason = r
	}
}

func (x *Explanation) decidedByRule(r ProductRule) {
	if x == nil {
		return
	}
	x.Reason = ReasonRule
	if n, ok := r.(NamedRule); ok {
		x.Rule = n.Name()
	}
}

func (x *Explanation) matched(subject string, b *Binding) {
	if x == nil {
		return
	}
	x.Reason, x.Subject, x.RoleID, x.Scope = ReasonBinding, subject, b.RoleID, b.Scope
}

func (x *Explanation) credentialRefused() {
	if x != nil {
		x.Reason, x.CredentialDenied = ReasonCredential, true
	}
}

// finish copies the chain out: a Source or ScopeResolver may reuse the buffer behind it once the decision is made.
func (x *Explanation) finish(allowed bool, chain []Scope) {
	if x == nil {
		return
	}
	x.Allowed = allowed
	if chain != nil {
		x.Chain = slices.Clone(chain)
	}
}
