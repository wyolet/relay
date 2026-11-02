// What a scope may link, asked row by row for a picker: each method is the check a save of the linking row runs on that one row, so a list filtered by it offers exactly what a save would accept. A nil error means usable; a refcheck *Error says why not.

package refcheck

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
)

// UsablePool checks the pool for a policy owned by by.
func (c Checker) UsablePool(ctx context.Context, p *pool.Pool, by meta.Owner) error {
	return c.linkRef(ctx, "policy", "pool", p.Meta, by)
}

// UsableBudget checks the budget for a policy owned by by.
func (c Checker) UsableBudget(ctx context.Context, b *budget.Budget, by meta.Owner) error {
	return c.linkRef(ctx, "policy", "budget", b.Meta, by)
}

// UsableRateLimit checks the rate limit for a policy owned by by.
func (c Checker) UsableRateLimit(ctx context.Context, rl *ratelimit.RateLimit, by meta.Owner) error {
	return c.rateLimitUsable(ctx, rl, by)
}

// UsableHostKey checks the host key for a pool owned by by; the pool's host is the list's host_id filter.
func (c Checker) UsableHostKey(ctx context.Context, k *hostkey.HostKey, by meta.Owner) error {
	return c.credentialUsable(ctx, "pool", k, by)
}

// UsablePolicy checks the policy, and the pools and keys it links, for a policy binding owned by by.
func (c Checker) UsablePolicy(ctx context.Context, p *policy.Policy, by meta.Owner) error {
	return c.policyBindable(ctx, p, by)
}

// UsableAccount checks the account for a policy binding owned by by. A host key joins only an account of its own owner, which the list's owner filters already express.
func (c Checker) UsableAccount(ctx context.Context, a *account.Account, by meta.Owner) error {
	return c.accountBindable(ctx, a, by)
}
