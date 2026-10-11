// The auth module's directory runs the writes of the account, tenancy and RBAC kinds. Relay's own rules (refcheck, the guards, keep-one-admin, admin-only role changes) still run first and answer with relay's status codes; the directory's checks then hold the same rows to its own invariants.

package control

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// directoryOps is the directory the handlers write through, or why there is none (a Deps without the stores).
type directoryOps struct {
	dir   *directory.Directory
	err   error
	authz authz.Authorizer
}

func newDirectoryOps(d Deps) directoryOps {
	ops := directoryOps{authz: d.Authz}
	if d.Stores == nil {
		ops.err = errors.New("directory: no stores are wired")
		return ops
	}
	cfg := directory.Config{
		Stores: d.Stores.Directory,
		Grants: grantChecker{authz: unaudited(d.Authz)},
		Kinds:  role.Kinds,
		Verbs:  role.Verbs,
	}
	if d.Sessions != nil {
		cfg.Revoker = d.Sessions
	}
	ops.dir, ops.err = directory.New(cfg)
	return ops
}

func (o directoryOps) directory() (*directory.Directory, error) {
	if o.err != nil {
		return nil, o.err
	}
	return o.dir, nil
}

// principalOf is the caller as the directory sees it; see refcheck.Principal.
func principalOf(ctx context.Context, az authz.Authorizer) *principal.Principal {
	return refcheck.Principal(ctx, az)
}

// grantChecker is the directory's grant check in relay; see refcheck.Grants.
type grantChecker struct{ authz authz.Authorizer }

func (g grantChecker) CheckGrant(ctx context.Context, p *principal.Principal, r rbac.Role, scope rbac.Scope, kinds, verbs []string) error {
	return refcheck.Grants{Authz: g.authz}.CheckGrant(ctx, p, r, scope, kinds, verbs)
}

// unaudited strips the audit wrapper: audit marks a request by its last mutating decision, and the directory's probes run after the handler's own Authorize.
func unaudited(a authz.Authorizer) authz.Authorizer {
	if w, ok := a.(audit.Authorizer); ok {
		return w.Inner
	}
	return a
}
