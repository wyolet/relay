package authz

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/auth/rbac"
)

// ErrNoExplanation is Explain's error from an authorizer that cannot say why it decides, such as the single-user one, which allows every authenticated caller everything.
var ErrNoExplanation = errors.New("authz: this authorizer does not explain its decisions")

// Explainer is an optional interface an Authorizer implements to answer an access review: the decision Authorize would make for a, who need not be the caller in ctx, and why.
type Explainer interface {
	Explain(ctx context.Context, a *actor.Actor, action string, res Resource) (rbac.Explanation, error)
}

// Explains reports whether a can answer an access review. A wrapper may implement Explainer only to delegate, so it is unwrapped (via an Unwrap() Authorizer method) down to the authorizer that decides.
func Explains(a Authorizer) bool {
	for {
		w, ok := a.(interface{ Unwrap() Authorizer })
		if !ok {
			break
		}
		a = w.Unwrap()
	}
	_, ok := a.(Explainer)
	return ok
}

// Explain implements Explainer over the same evaluation Authorize runs, so the two cannot disagree.
func (r RBAC) Explain(ctx context.Context, a *actor.Actor, action string, res Resource) (rbac.Explanation, error) {
	if !a.IsAuthenticated() {
		return rbac.Explanation{Reason: rbac.ReasonNone}, ErrUnauthenticated
	}
	ev := acquireEvaluation(r.Snap, a)
	defer ev.release()
	engine := ev.engine()
	return engine.Explain(ctx, &ev.principal, action, ev.resource(res))
}

// MatchedBinding returns the RoleBinding row behind an explanation's binding: the first of the subject's bindings with that role and scope, which is the one the evaluation read first.
func MatchedBinding(snap Snapshot, x rbac.Explanation) (*rolebinding.RoleBinding, bool) {
	if snap == nil || x.RoleID == "" {
		return nil, false
	}
	scope := meta.Owner{Kind: meta.OwnerKind(x.Scope.Kind), ID: x.Scope.ID}
	for _, b := range snap.RoleBindingsForSubject(x.Subject) {
		if b.Spec.RoleID == x.RoleID && b.Spec.Scope == scope {
			return b, true
		}
	}
	return nil, false
}
