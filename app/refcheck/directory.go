package refcheck

import (
	"context"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// Principal is the caller in ctx as the auth module's directory sees it. Admin is RequireAdmin's rule (the admin role or token, any caller in single-user mode, or a loader with no authorizer), so the directory's admin-only paths open exactly where relay's do.
func Principal(ctx context.Context, a authz.Authorizer) *principal.Principal {
	var p principal.Principal
	if act := actor.From(ctx); act != nil {
		p = act.Principal()
	}
	p.Admin = RequireAdmin(ctx, a, "") == nil
	return &p
}

// Grants is the directory's grant check in relay: authz.CheckGrant, which expands wildcards over relay's closed vocabulary and probes through Authz. An admin passes without probing.
type Grants struct{ Authz authz.Authorizer }

func (g Grants) CheckGrant(ctx context.Context, p *principal.Principal, r rbac.Role, scope rbac.Scope, _, _ []string) error {
	if p != nil && p.Admin {
		return nil
	}
	rules := make([]role.Rule, len(r.Rules))
	for i, rule := range r.Rules {
		rules[i] = role.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
	}
	return authz.CheckGrant(ctx, g.Authz, &role.Role{Meta: meta.Metadata{ID: r.ID, Name: r.ID}, Spec: role.Spec{Rules: rules}},
		meta.Owner{Kind: meta.OwnerKind(scope.Kind), ID: scope.ID})
}
