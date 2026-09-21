// Package authz answers "can this actor perform this action on this
// resource?" — a deliberate seam so the v1 always-allow implementation
// can later be swapped for Casbin / OpenFGA / Keto without touching call
// sites.
//
// Handlers call:
//
//	if err := d.Authz.Authorize(ctx, "policy.write", authz.Resource{...}); err != nil {
//	    return nil, err
//	}
//
// before mutating state. Today the call is a near-no-op for any
// authenticated caller; tomorrow it gates the action against per-org
// permissions, ABAC predicates, or whatever the SaaS tier needs.
package authz

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
)

// ErrUnauthenticated is returned when the call has no actor in context.
// Mapped to HTTP 401 by the control-plane middleware.
var ErrUnauthenticated = errors.New("authz: unauthenticated")

// ErrForbidden is returned when an authenticated actor lacks permission.
// Mapped to HTTP 403.
var ErrForbidden = errors.New("authz: forbidden")

// Resource describes the target of an authorization check. Fields are
// optional; populate what the action needs.
type Resource struct {
	Kind string // "provider", "policy", "model", ...
	ID   string // resource id, if known (UUID)
	Name string // resource slug, if id is not known
	// Owner is the target row's provenance, populated by handlers that
	// have already fetched the row (mutations authorize twice as cheaply
	// as fetching twice). Nil means "owner unknown" — owner-aware
	// implementations must fail closed on mutations, not open.
	Owner *meta.Owner
}

// Scoper is an optional interface an Authorizer may implement to gate
// per-row visibility in list/read paths, where per-row Authorize calls
// would be the wrong shape. Handlers filter rows through Visible when the
// configured Authorizer implements it; an Authorizer that doesn't (the
// default AlwaysAllowAuthenticated) leaves reads unfiltered.
type Scoper interface {
	// Visible reports whether the current actor may see a row of kind
	// with the given id and owner in lists and reads. id is the row's own
	// metadata.id — it is what puts a scope-defining row (Team, Project)
	// inside the scope it defines; pass "" when there is no row.
	Visible(ctx context.Context, kind, id string, owner meta.Owner) bool
}

// Authorizer is the policy-decision interface. Authorize returns nil to
// allow, ErrForbidden/ErrUnauthenticated to deny, or any other error to
// signal a backend failure (which the middleware should treat as 500, not
// 403 — failure to authorize is not the same as denial).
type Authorizer interface {
	Authorize(ctx context.Context, action string, resource Resource) error
}

// AlwaysAllowAuthenticated is the v1 implementation: any authenticated
// caller (user session or admin-token bypass) is granted any action.
// Unauthenticated callers are rejected.
//
// This exists so handler call sites are permanent. Swap the concrete type
// when multi-tenant work lands; handlers stay the same.
type AlwaysAllowAuthenticated struct{}

// Authorize implements Authorizer.
func (AlwaysAllowAuthenticated) Authorize(ctx context.Context, _ string, _ Resource) error {
	if !actor.From(ctx).IsAuthenticated() {
		return ErrUnauthenticated
	}
	return nil
}
