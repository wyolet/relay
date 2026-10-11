// The auth module's directory runs the writes of the account, tenancy and RBAC kinds. Relay's own rules (refcheck, the guards, keep-one-admin, admin-only role changes) still run first and answer with relay's status codes; the directory's checks then hold the same rows to its own invariants.

package control

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
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

// principalOf is the caller as the directory sees it. Admin is relay's RequireAdmin rule (the admin role or token, or any caller in single-user mode), so the directory's admin-only paths open exactly where relay's do.
func principalOf(ctx context.Context, az authz.Authorizer) *principal.Principal {
	var p principal.Principal
	if a := actor.From(ctx); a != nil {
		p = a.Principal()
	}
	p.Admin = refcheck.RequireAdmin(ctx, az, "") == nil
	return &p
}

// grantChecker is the directory's grant check in relay: relay's own CheckGrant, which expands wildcards over the closed vocabulary and probes through the authorizer.
type grantChecker struct{ authz authz.Authorizer }

func (g grantChecker) CheckGrant(ctx context.Context, p *principal.Principal, r rbac.Role, scope rbac.Scope, _, _ []string) error {
	if p != nil && p.Admin {
		return nil
	}
	rules := make([]role.Rule, len(r.Rules))
	for i, rule := range r.Rules {
		rules[i] = role.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
	}
	return authz.CheckGrant(ctx, g.authz, &role.Role{Meta: meta.Metadata{ID: r.ID, Name: r.ID}, Spec: role.Spec{Rules: rules}},
		meta.Owner{Kind: meta.OwnerKind(scope.Kind), ID: scope.ID})
}

// unaudited strips the audit wrapper: audit marks a request by its last mutating decision, and the directory's probes run after the handler's own Authorize.
func unaudited(a authz.Authorizer) authz.Authorizer {
	if w, ok := a.(audit.Authorizer); ok {
		return w.Inner
	}
	return a
}
