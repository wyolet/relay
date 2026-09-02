package audit

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
)

// Authorizer wraps the configured authorizer and feeds every decision into
// the in-flight event. Wrapping is what keeps handlers free of audit calls:
// the action string and resource a handler already passes to Authorize are
// exactly what the row needs.
type Authorizer struct {
	Inner authz.Authorizer
	// Snap resolves the current catalog snapshot for the owner's scope
	// chain. nil leaves Resource.Scope empty.
	Snap func() *catalog.Snapshot
}

// Authorize implements authz.Authorizer, delegating verbatim.
func (a Authorizer) Authorize(ctx context.Context, action string, res authz.Resource) error {
	err := a.Inner.Authorize(ctx, action, res)
	if f := fromContext(ctx); f != nil {
		f.add(decision{
			Action: action,
			Resource: Resource{
				Kind:  res.Kind,
				ID:    res.ID,
				Name:  res.Name,
				Owner: res.Owner,
				Scope: a.scopeOf(res.Owner),
			},
			Status: statusOf(err),
		})
	}
	return err
}

// Visible implements authz.Scoper by delegation. An inner authorizer that
// doesn't scope reads stays unscoped — wrapping must not change what a
// caller can see.
func (a Authorizer) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	s, ok := a.Inner.(authz.Scoper)
	if !ok {
		return true
	}
	return s.Visible(ctx, kind, id, owner)
}

// scopeOf renders the owner's scope chain as "<kind>:<id>", most specific
// first. The global scope every chain ends in carries no information and is
// dropped.
func (a Authorizer) scopeOf(owner *meta.Owner) []string {
	if a.Snap == nil {
		return nil
	}
	snap := a.Snap()
	if snap == nil {
		return nil
	}
	var out []string
	for _, o := range snap.ScopeChain(owner) {
		if o.Kind == meta.OwnerSystem || o.ID == "" {
			continue
		}
		out = append(out, string(o.Kind)+":"+o.ID)
	}
	return out
}

// statusOf maps an Authorize result onto an outcome status. A backend
// failure is an error, not a denial.
func statusOf(err error) string {
	switch {
	case err == nil:
		return StatusAllowed
	case errors.Is(err, authz.ErrForbidden), errors.Is(err, authz.ErrUnauthenticated):
		return StatusDenied
	default:
		return StatusError
	}
}
