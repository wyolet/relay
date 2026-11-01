// A rate limit linked with a models scope counts per model, which only the upstream rungs (host, account, credential) do; a caller rung's limits and budgets cover the whole policy. These rules keep a model-scoped limit off every caller subject, from either side: binding the policy, or adding the scope to a bound policy.

package refcheck

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// callerSubject reports a subject on the caller side of the ladder.
func callerSubject(k rolebinding.SubjectKind) bool {
	switch k {
	case rolebinding.SubjectUser, rolebinding.SubjectServiceAccount, policybinding.SubjectTeam, policybinding.SubjectProject:
		return true
	}
	return false
}

// modelScopedLimit returns the first rate-limit link of p that names models.
func modelScopedLimit(p *policy.Policy) (policy.RateLimitLink, bool) {
	for _, l := range p.Spec.RateLimits {
		if len(l.Models) > 0 {
			return l, true
		}
	}
	return policy.RateLimitLink{}, false
}

// refuseModelScopeOnCaller answers 400 when p links a model-scoped rate limit and sub is a caller subject.
func (c Checker) refuseModelScopeOnCaller(ctx context.Context, p *policy.Policy, sub rolebinding.Subject, binding string) error {
	if !callerSubject(sub.Kind) {
		return nil
	}
	l, ok := modelScopedLimit(p)
	if !ok {
		return nil
	}
	limit := l.RateLimitID
	if c.Rows.RateLimit != nil {
		if rl := c.Rows.RateLimit(ctx, l.RateLimitID); rl != nil {
			limit = rl.Meta.Name
		}
	}
	return badRequest("policy %q links rate limit %q scoped to models [%s], and %s binds it to %s; a models scope applies only on a host, account or credential, while a %s's limits cover the whole policy",
		p.Meta.Name, limit, strings.Join(l.Models, ", "), binding, sub.Key(), sub.Kind)
}

// modelScopeOnBinding checks the subjects of pb against the policy it uses.
func (c Checker) modelScopeOnBinding(ctx context.Context, pb *policybinding.PolicyBinding) error {
	if c.Rows.Policy == nil {
		return nil
	}
	p := c.Rows.Policy(ctx, pb.Spec.PolicyID)
	if p == nil {
		return nil
	}
	for _, sub := range pb.Spec.Subjects {
		if err := c.refuseModelScopeOnCaller(ctx, p, sub, "this binding"); err != nil {
			return err
		}
	}
	return nil
}

// modelScopeOnPolicy checks the bindings already using p, so a policy bound to a caller subject cannot gain a models scope.
func (c Checker) modelScopeOnPolicy(ctx context.Context, p *policy.Policy) error {
	if c.Rows.PolicyBindingsFor == nil || p.Meta.ID == "" {
		return nil
	}
	if _, ok := modelScopedLimit(p); !ok {
		return nil
	}
	bindings, err := c.Rows.PolicyBindingsFor(ctx, p.Meta.ID)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: err.Error(), Err: err}
	}
	for _, pb := range bindings {
		by := "another policy binding"
		if Visible(ctx, c.Authz, "policy-binding", pb.Meta.ID, pb.Meta.Owner) {
			by = fmt.Sprintf("policy binding %q", pb.Meta.Name)
		}
		for _, sub := range pb.Spec.Subjects {
			if err := c.refuseModelScopeOnCaller(ctx, p, sub, by); err != nil {
				return err
			}
		}
	}
	return nil
}
