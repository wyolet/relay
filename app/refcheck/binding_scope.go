// Scope rules for a policy binding: it is owned by the system, a team, a project or a user, it binds only accounts inside that scope, and it uses a policy of that scope, an ancestor of it, the system, a host, or a user who may manage policy bindings there, whose linked pools that scope may use.

package refcheck

import (
	"context"
	"net/http"
	"strconv"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
)

// BindablePolicy checks the policy a binding owned by scope uses. A host's tier preset is shared like a system policy (a team binds the provider's tier to its own account). A personal policy is bound by its owner: to their own accounts, or from a team or project where its owner may manage policy bindings.
func (c Checker) BindablePolicy(ctx context.Context, policyID string, scope meta.Owner) error {
	if policyID == "" || c.Rows.Policy == nil {
		return nil
	}
	p := c.Rows.Policy(ctx, policyID)
	if p == nil {
		return badRequest("policy %q does not exist", policyID)
	}
	if !Visible(ctx, c.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
		return notVisible("policy", policyID)
	}
	switch {
	case p.Meta.Owner.Kind == meta.OwnerUser:
		if err := c.personalPolicyBindableIn(ctx, p, scope); err != nil {
			return err
		}
	case p.Meta.Owner.Kind != meta.OwnerHost && !c.containsScope(ctx, p.Meta.Owner, scope):
		return badRequest("policy %q is owned by %s; a binding owned by %s may use a policy of its own scope, an ancestor of it, the system, a host, or a user who may manage policy bindings there", p.Meta.Name, ownerText(p.Meta.Owner), ownerText(scope))
	}
	return c.linksUsableIn(ctx, p, scope)
}

// personalPolicyBindableIn refuses a user's policy bound outside what that user controls: their own binding, or a team or project where they may manage policy bindings. Without a permission lookup (a loader running as the deployment) a team or project may bind it; the plans re-check.
func (c Checker) personalPolicyBindableIn(ctx context.Context, p *policy.Policy, scope meta.Owner) error {
	owner := p.Meta.Owner
	switch scope.Kind {
	case meta.OwnerUser:
		if scope.ID == owner.ID {
			return nil
		}
	case meta.OwnerTeam, meta.OwnerProject:
		if c.Rows.UserMayBindIn == nil {
			return nil
		}
		ok, err := c.Rows.UserMayBindIn(ctx, owner.ID, scope)
		if err != nil {
			return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
		}
		if ok {
			return nil
		}
		return badRequest("policy %q is owned by %s, who may not manage policy bindings in %s", p.Meta.Name, ownerText(owner), ownerText(scope))
	}
	return badRequest("policy %q is owned by %s; a binding owned by %s cannot use it: a personal policy is bound by its owner, or by a team or project where its owner may manage policy bindings", p.Meta.Name, ownerText(owner), ownerText(scope))
}

// linksUsableIn refuses a policy linking a pool, or a pool holding a credential or drawing on an account, that scope may not use: binding the policy would hand the scope's accounts another scope's credentials. A missing link is left to the plan, where it narrows.
func (c Checker) linksUsableIn(ctx context.Context, p *policy.Policy, scope meta.Owner) error {
	if c.Rows.Pool == nil {
		return nil
	}
	for _, id := range p.Spec.PoolIDs {
		pl := c.Rows.Pool(ctx, id)
		if pl == nil {
			continue
		}
		name := c.linkName(ctx, "pool", pl.Meta)
		if !c.containsScope(ctx, pl.Meta.Owner, scope) {
			return badRequest("policy %q links pool %s owned by %s, which a binding owned by %s cannot use", p.Meta.Name, name, ownerText(pl.Meta.Owner), ownerText(scope))
		}
		if c.Rows.Account != nil {
			for _, aid := range pl.Spec.AccountIDs {
				a := c.Rows.Account(ctx, aid)
				if a != nil && !c.containsScope(ctx, a.Meta.Owner, scope) {
					return badRequest("policy %q links pool %s drawing on account %s owned by %s, which a binding owned by %s cannot use", p.Meta.Name, name, c.linkName(ctx, "account", a.Meta), ownerText(a.Meta.Owner), ownerText(scope))
				}
			}
		}
		if c.Rows.HostKey == nil {
			continue
		}
		for _, kid := range pl.Spec.CredentialIDs {
			k := c.Rows.HostKey(ctx, kid)
			if k != nil && !c.containsScope(ctx, k.Meta.Owner, scope) {
				return badRequest("policy %q links pool %s holding host-key %s owned by %s, which a binding owned by %s cannot use", p.Meta.Name, name, c.linkName(ctx, "host-key", k.Meta), ownerText(k.Meta.Owner), ownerText(scope))
			}
		}
	}
	return nil
}

// linkName names a linked row by name when the caller may see it, else by id.
func (c Checker) linkName(ctx context.Context, kind string, m meta.Metadata) string {
	if Visible(ctx, c.Authz, kind, m.ID, m.Owner) {
		return strconv.Quote(m.Name)
	}
	return strconv.Quote(m.ID)
}

// linksOnPolicy checks the bindings already using p against its links, so a bound policy cannot gain a pool its bindings' scopes may not use.
func (c Checker) linksOnPolicy(ctx context.Context, p *policy.Policy) error {
	if c.Rows.PolicyBindingsFor == nil || p.Meta.ID == "" || len(p.Spec.PoolIDs) == 0 {
		return nil
	}
	bindings, err := c.Rows.PolicyBindingsFor(ctx, p.Meta.ID)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	for _, pb := range bindings {
		if err := c.linksUsableIn(ctx, p, pb.Meta.Owner); err != nil {
			return err
		}
	}
	return nil
}

// accountsInScope refuses an account that does not exist, that the caller may not see, or that lives outside the binding owner's scope: a project binds its own account and the accounts it owns (its members', its service accounts', its provider accounts); a team the same for itself and its projects; a user the accounts they own; the system any account.
func (c Checker) accountsInScope(ctx context.Context, pb *policybinding.PolicyBinding) error {
	if c.Rows.Account == nil {
		return nil
	}
	owner := pb.Meta.Owner
	for i, id := range pb.Spec.AccountIDs {
		a := c.Rows.Account(ctx, id)
		if a == nil {
			return badRequest("accountIds[%d]: account %q does not exist", i, id)
		}
		if !Visible(ctx, c.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			return notVisible("account", id)
		}
		if !c.containsScope(ctx, owner, a.Meta.Owner) {
			return badRequest("accountIds[%d]: account %q is owned by %s, outside the scope of this binding's owner, %s", i, a.Meta.Name, ownerText(a.Meta.Owner), ownerText(owner))
		}
	}
	return nil
}

// containsScope reports whether scope outer holds scope inner: the same scope, the system above everything, or a team above its projects. Without a project lookup a team is taken to hold every project.
func (c Checker) containsScope(ctx context.Context, outer, inner meta.Owner) bool {
	switch {
	case outer.Kind == meta.OwnerSystem, outer == inner:
		return true
	case outer.Kind == meta.OwnerTeam && inner.Kind == meta.OwnerProject:
		return c.projectInTeam(ctx, inner.ID, outer.ID)
	}
	return false
}

func ownerText(o meta.Owner) string {
	if o.ID == "" {
		return string(o.Kind)
	}
	return string(o.Kind) + " " + o.ID
}
