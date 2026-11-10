package control

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// droppedLinkScans maps a kind to the scan listing the rows a delete of one of its rows takes a link from without refusing it: a rate limit or budget a policy stops counting, or a pool a policy stops reaching (the policy keeps the id and narrows, never widens). A host key drops out of the pools holding it, an account out of the pools drawing on it, and the shares a team, project or service account received out of theirs.
var droppedLinkScans = map[string]func(ctx context.Context, d Deps, id string) ([]referenceItem, error){
	"host-key": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return poolsHolding(ctx, d, id)
	},
	"account": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return poolsDrawingOn(ctx, d, id)
	},
	"team": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return poolsLosingShares(ctx, d, sharesOwnedBy(meta.OwnerTeam, id))
	},
	"project": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return poolsLosingShares(ctx, d, sharesOwnedBy(meta.OwnerProject, id))
	},
	"service-account": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return poolsLosingShares(ctx, d, sharesHeldBy(id))
	},
	"rate-limit": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return policiesLinking(ctx, d, "spec.rateLimits[].rateLimitId", func(p *policy.Policy) bool {
			return slices.ContainsFunc(p.Spec.RateLimits, func(l policy.RateLimitLink) bool { return l.RateLimitID == id })
		})
	},
	"pool": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return policiesLinking(ctx, d, "spec.poolIds", func(p *policy.Policy) bool { return slices.Contains(p.Spec.PoolIDs, id) })
	},
	"budget": func(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
		return policiesLinking(ctx, d, "spec.budgets[].budgetId", func(p *policy.Policy) bool { return slices.Contains(p.BudgetIDs(), id) })
	},
}

func policiesLinking(ctx context.Context, d Deps, via string, links func(*policy.Policy) bool) ([]referenceItem, error) {
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	var out []referenceItem
	for _, p := range pols {
		if links(p) {
			out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: via, owner: p.Meta.Owner})
		}
	}
	return out, nil
}
