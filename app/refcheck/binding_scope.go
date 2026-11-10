// Scope rules for a policy binding: it is owned by the system, a team, a project or a user, it binds only accounts inside that scope, and it uses a policy of that scope, an ancestor of it, the system, a host, or a user who may manage policy bindings there, whose linked pools that scope may use.

package refcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/meta"
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
	return c.policyBindable(ctx, p, scope)
}

func (c Checker) policyBindable(ctx context.Context, p *policy.Policy, scope meta.Owner) error {
	if !Visible(ctx, c.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
		return notVisible("policy", p.Meta.ID)
	}
	switch {
	case p.Meta.Owner.Kind == meta.OwnerUser:
		if err := c.personalPolicyBindableIn(ctx, p, scope); err != nil {
			return err
		}
	case p.Meta.Owner.Kind != meta.OwnerHost && !c.containsScope(ctx, p.Meta.Owner, scope):
		return badRequest("a policy binding of %s cannot use %s: %s binding can use policies of %s, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in %s, or move this one there.",
			c.scopeText(ctx, scope), c.rowText(ctx, "policy", p.Meta), scopeArticle(scope), reachText(scope), homeText(scope))
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
		return badRequest("%s cannot be bound in %s: %s may not manage policy bindings there. Use a policy of %s instead.",
			c.rowText(ctx, "policy", p.Meta), c.scopeText(ctx, scope), c.userSubject(ctx, owner.ID), reachText(scope))
	}
	return badRequest("a policy binding of %s cannot use %s: a personal policy is bound by its owner to their own accounts, or in a team or project where its owner may manage policy bindings. Use a policy of %s instead.",
		c.scopeText(ctx, scope), c.rowText(ctx, "policy", p.Meta), reachText(scope))
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
		poolText := c.rowText(ctx, "pool", pl.Meta)
		if !c.containsScope(ctx, pl.Meta.Owner, scope) {
			return badRequest("policy %q uses %s; a policy binding of %s can use pools of %s. Create the pool in %s, or move this one there.",
				label(p.Meta), poolText, c.scopeText(ctx, scope), reachText(scope), homeText(scope))
		}
		if c.Rows.Account != nil {
			for _, aid := range pl.Spec.AccountIDs {
				a := c.Rows.Account(ctx, aid)
				if a != nil && !c.containsScope(ctx, a.Meta.Owner, scope) {
					return badRequest("policy %q uses %s, which draws on %s; a policy binding of %s can use accounts of %s. Take the account out of the pool, or share it with %s.",
						label(p.Meta), poolText, c.rowText(ctx, "account", a.Meta), c.scopeText(ctx, scope), reachText(scope), c.scopeText(ctx, scope))
				}
			}
		}
		if c.Rows.HostKey == nil {
			continue
		}
		for _, kid := range pl.Spec.CredentialIDs {
			k := c.Rows.HostKey(ctx, kid)
			if k != nil && !c.containsScope(ctx, k.Meta.Owner, scope) {
				return badRequest("policy %q uses %s, which holds %s; a policy binding of %s can use host keys of %s. Take the key out of the pool, or move it to %s.",
					label(p.Meta), poolText, c.rowText(ctx, "host-key", k.Meta), c.scopeText(ctx, scope), reachText(scope), homeText(scope))
			}
		}
	}
	return nil
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

// accountsInScope refuses an account that does not exist, that the caller may not see, or that lives outside the binding owner's scope.
func (c Checker) accountsInScope(ctx context.Context, pb *policybinding.PolicyBinding) error {
	if c.Rows.Account == nil {
		return nil
	}
	for i, id := range pb.Spec.AccountIDs {
		a := c.Rows.Account(ctx, id)
		if a == nil {
			return badRequest("accountIds[%d]: account %q does not exist", i, id)
		}
		if err := c.accountBindable(ctx, a, pb.Meta.Owner); err != nil {
			var re *Error
			if errors.As(err, &re) && re.Status == http.StatusBadRequest {
				re.Msg = fmt.Sprintf("accountIds[%d]: %s", i, re.Msg)
			}
			return err
		}
	}
	return nil
}

// accountBindable is the scope rule for one account a binding owned by owner binds: a project binds its own account and the accounts it owns (its members', its service accounts', its provider accounts); a team the same for itself and its projects; a user the accounts they own; the system any account.
func (c Checker) accountBindable(ctx context.Context, a *account.Account, owner meta.Owner) error {
	if !Visible(ctx, c.Authz, "account", a.Meta.ID, a.Meta.Owner) {
		return notVisible("account", a.Meta.ID)
	}
	if c.containsScope(ctx, owner, a.Meta.Owner) {
		return nil
	}
	return badRequest("%s is outside the scope of this binding's owner, %s: %s. Create the binding in %s instead.",
		c.rowText(ctx, "account", a.Meta), c.scopeText(ctx, owner), bindReachText(owner), c.scopeText(ctx, a.Meta.Owner))
}

// bindReachText says which accounts a binding owned by owner binds.
func bindReachText(owner meta.Owner) string {
	switch owner.Kind {
	case meta.OwnerProject:
		return "a project's binding binds the project's own accounts: its account, its members', its service accounts' and its provider accounts"
	case meta.OwnerTeam:
		return "a team's binding binds the accounts of the team and its projects"
	case meta.OwnerUser:
		return "a personal binding binds only its owner's own accounts"
	}
	return "a system binding binds any account"
}

// containsScope reports whether scope outer holds scope inner: the same scope, the system above everything, or a team above its projects. Without a project lookup a team is taken to hold every project.
func (c Checker) containsScope(ctx context.Context, outer, inner meta.Owner) bool {
	if c.Rows.Project == nil && outer.Kind == meta.OwnerTeam && inner.Kind == meta.OwnerProject {
		return true
	}
	return outer.Holds(inner, func(projectID string) (string, bool) {
		if p := c.Rows.Project(ctx, projectID); p != nil {
			return p.Spec.TeamID, true
		}
		return "", false
	})
}
