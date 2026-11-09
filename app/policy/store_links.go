// store_links.go reads and writes a policy's pool, rate-limit and budget junctions (migrations 0035, 0039).

package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/internal/storage/gen"
)

// links holds every policy's pool, rate-limit and budget junction rows, keyed by policy id.
type links struct {
	pools      map[string][]string
	rateLimits map[string][]RateLimitLink
	budgets    map[string][]BudgetLink
}

func listLinks(ctx context.Context, q *gen.Queries) (links, error) {
	l := links{pools: map[string][]string{}, rateLimits: map[string][]RateLimitLink{}, budgets: map[string][]BudgetLink{}}
	pools, err := q.ListPolicyPools(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List pools: %w", err)
	}
	for _, r := range pools {
		l.pools[r.PolicyID] = append(l.pools[r.PolicyID], r.PoolID)
	}
	rls, err := q.ListPolicyRateLimits(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models, r.Shared)
		if err != nil {
			return l, fmt.Errorf("policy %s rateLimits: %w", r.PolicyID, err)
		}
		l.rateLimits[r.PolicyID] = append(l.rateLimits[r.PolicyID], link)
	}
	budgets, err := q.ListPolicyBudgets(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List budgets: %w", err)
	}
	for _, r := range budgets {
		l.budgets[r.PolicyID] = append(l.budgets[r.PolicyID], BudgetLink{BudgetID: r.BudgetID, Shared: &r.Shared})
	}
	return l, nil
}

// getLinks fills spec's pool, rate-limit and budget lists for one policy.
func getLinks(ctx context.Context, q *gen.Queries, id string, spec *Spec) error {
	pools, err := q.GetPolicyPools(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get pools: %w", err)
	}
	for _, r := range pools {
		spec.PoolIDs = append(spec.PoolIDs, r.PoolID)
	}
	rls, err := q.GetPolicyRateLimits(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models, r.Shared)
		if err != nil {
			return fmt.Errorf("policy.Get rateLimits: %w", err)
		}
		spec.RateLimits = append(spec.RateLimits, link)
	}
	budgets, err := q.GetPolicyBudgets(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get budgets: %w", err)
	}
	for _, r := range budgets {
		spec.Budgets = append(spec.Budgets, BudgetLink{BudgetID: r.BudgetID, Shared: &r.Shared})
	}
	return nil
}

// writeLinks replaces p's pool, rate-limit and budget junction rows; q must be inside the Upsert transaction.
func writeLinks(ctx context.Context, q *gen.Queries, p *Policy) error {
	id := p.Meta.ID
	if err := q.DeletePolicyPools(ctx, id); err != nil {
		return fmt.Errorf("clear pools: %w", err)
	}
	for i, poolID := range p.Spec.PoolIDs {
		if err := q.InsertPolicyPool(ctx, gen.InsertPolicyPoolParams{PolicyID: id, PoolID: poolID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert pool %s: %w", poolID, err)
		}
	}
	if err := q.DeletePolicyRateLimits(ctx, id); err != nil {
		return fmt.Errorf("clear rateLimits: %w", err)
	}
	for i, l := range p.Spec.RateLimits {
		models := []byte("[]")
		if len(l.Models) > 0 {
			var err error
			if models, err = json.Marshal(l.Models); err != nil {
				return fmt.Errorf("rateLimits[%d] models: %w", i, err)
			}
		}
		if err := q.InsertPolicyRateLimit(ctx, gen.InsertPolicyRateLimitParams{PolicyID: id, RateLimitID: l.RateLimitID, Models: models, Position: int32(i), Shared: l.IsShared()}); err != nil {
			return fmt.Errorf("insert rate limit %s: %w", l.RateLimitID, err)
		}
	}
	if err := q.DeletePolicyBudgets(ctx, id); err != nil {
		return fmt.Errorf("clear budgets: %w", err)
	}
	for i, b := range p.Spec.Budgets {
		if err := q.InsertPolicyBudget(ctx, gen.InsertPolicyBudgetParams{PolicyID: id, BudgetID: b.BudgetID, Position: int32(i), Shared: b.IsShared()}); err != nil {
			return fmt.Errorf("insert budget %s: %w", b.BudgetID, err)
		}
	}
	return nil
}

func rateLimitLinkFromRow(rateLimitID string, models []byte, shared bool) (RateLimitLink, error) {
	link := RateLimitLink{RateLimitID: rateLimitID, Shared: &shared}
	if err := json.Unmarshal(models, &link.Models); err != nil {
		return link, fmt.Errorf("models: %w", err)
	}
	// An unscoped link reads back as nil, as it was written.
	if len(link.Models) == 0 {
		link.Models = nil
	}
	return link, nil
}
