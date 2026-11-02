// What a scope may link, asked row by row for a picker: each method runs the save check of the linking row, as a row owned by the scope naming that one row, so a list filtered by it offers exactly what a save would accept. A nil error means usable; a refcheck *Error says why not.

package refcheck

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
)

// UsableScope checks the scope a picker asks for as a save checks the owner it writes: a team or project must exist (400) and be visible (404), and a user is the caller unless the caller is the admin token (400).
func (c Checker) UsableScope(ctx context.Context, by meta.Owner) error {
	if by.Kind == meta.OwnerUser {
		o := by
		if err := StampOwnerID(ctx, &o); err != nil {
			return badRequest("%s", err.Error())
		}
		return nil
	}
	return c.ownerScopeRef(ctx, by)
}

// UsablePool checks the pool for a policy owned by by.
func (c Checker) UsablePool(ctx context.Context, p *pool.Pool, by meta.Owner) error {
	return c.PoolRefs(ctx, []string{p.Meta.ID}, by)
}

// UsableBudget checks the budget for a policy owned by by.
func (c Checker) UsableBudget(ctx context.Context, b *budget.Budget, by meta.Owner) error {
	return c.BudgetRefs(ctx, []string{b.Meta.ID}, by)
}

// UsableRateLimit checks the rate limit for a policy owned by by.
func (c Checker) UsableRateLimit(ctx context.Context, rl *ratelimit.RateLimit, by meta.Owner) error {
	return c.RateLimitRef(ctx, rl.Meta.ID, by)
}

// UsableHostKey checks the host key for a pool owned by by at the key's own host; the pool's host is the list's host_id filter.
func (c Checker) UsableHostKey(ctx context.Context, k *hostkey.HostKey, by meta.Owner) error {
	return c.credentialRefs(ctx, "pool", []string{k.Meta.ID}, by, k.Spec.HostID)
}

// UsablePolicy checks the policy, and the pools and keys it links, for a new policy binding owned by by.
func (c Checker) UsablePolicy(ctx context.Context, p *policy.Policy, by meta.Owner) error {
	return c.PolicyBinding(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{Owner: by}, Spec: policybinding.Spec{PolicyID: p.Meta.ID}})
}

// UsableAccount checks the account for a new policy binding owned by by: in its scope and held by no other binding. A host key joins only an account of its own owner, which the list's owner filters already express.
func (c Checker) UsableAccount(ctx context.Context, a *account.Account, by meta.Owner) error {
	return c.PolicyBinding(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{Owner: by}, Spec: policybinding.Spec{AccountIDs: []string{a.Meta.ID}}})
}

// UsablePoolAccount checks the account for a pool owned by by at the account's own host: its own and reachable provider accounts and the shares made with by; the pool's host is the list's host_id filter.
func (c Checker) UsablePoolAccount(ctx context.Context, a *account.Account, by meta.Owner) error {
	return c.PoolAccountRefs(ctx, []string{a.Meta.ID}, by, a.Spec.HostID)
}
