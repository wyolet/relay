package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/pool"
)

// guardAccount checks the account's host and refuses an owner move that
// would leave a host key naming an account it may not use.
func guardAccount(d Deps) mutationGuard[account.Account] {
	return func(ctx context.Context, action string, existing, incoming *account.Account) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if err := refs(d).Account(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Account = func(context.Context, string) *account.Account { return incoming }
		return refuseStrandingMove(ctx, d, "account", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			return it.Kind == "host-key" && c.AccountRef(ctx, incoming.Meta.ID, it.owner) == nil
		})
	}
}

// guardPool checks the pool's host and keys, and refuses an owner move away
// from the policies drawing on it.
func guardPool(d Deps) mutationGuard[pool.Pool] {
	return func(ctx context.Context, action string, existing, incoming *pool.Pool) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if err := refs(d).Pool(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Pool = func(context.Context, string) *pool.Pool { return incoming }
		return refuseStrandingMove(ctx, d, "pool", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			return it.Kind == "policy" && c.PoolRefs(ctx, []string{incoming.Meta.ID}, it.owner) == nil
		})
	}
}

// guardBudgetMove refuses an owner move away from the policies counting
// against the budget.
func guardBudgetMove(d Deps) mutationGuard[budget.Budget] {
	return func(ctx context.Context, action string, existing, incoming *budget.Budget) error {
		if action != "update" || existing.Meta.Owner == incoming.Meta.Owner {
			return nil
		}
		c := refs(d)
		c.Rows.Budget = func(context.Context, string) *budget.Budget { return incoming }
		return refuseStrandingMove(ctx, d, "budget", incoming.Meta.ID, incoming.Meta.Name, incoming.Meta.Owner, func(it referenceItem) bool {
			return it.Kind == "policy" && c.BudgetRefs(ctx, []string{incoming.Meta.ID}, it.owner) == nil
		})
	}
}
