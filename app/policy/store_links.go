// store_links.go reads and writes a policy's pool, credential, rate-limit and budget junctions (migration 0035).

package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/internal/storage/gen"
)

// links holds every policy's pool, credential, rate-limit and budget junction rows, keyed by policy id.
type links struct {
	pools, credentials, budgets map[string][]string
	rateLimits                  map[string][]RateLimitLink
}

func listLinks(ctx context.Context, q *gen.Queries) (links, error) {
	l := links{pools: map[string][]string{}, credentials: map[string][]string{}, budgets: map[string][]string{}, rateLimits: map[string][]RateLimitLink{}}
	pools, err := q.ListPolicyPools(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List pools: %w", err)
	}
	for _, r := range pools {
		l.pools[r.PolicyID] = append(l.pools[r.PolicyID], r.PoolID)
	}
	creds, err := q.ListPolicyCredentials(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List credentials: %w", err)
	}
	for _, r := range creds {
		l.credentials[r.PolicyID] = append(l.credentials[r.PolicyID], r.HostKeyID)
	}
	rls, err := q.ListPolicyRateLimits(ctx)
	if err != nil {
		return l, fmt.Errorf("policy.List rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models)
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
		l.budgets[r.PolicyID] = append(l.budgets[r.PolicyID], r.BudgetID)
	}
	return l, nil
}

// getLinks fills spec's pool, credential, rate-limit and budget lists for one policy.
func getLinks(ctx context.Context, q *gen.Queries, id string, spec *Spec) error {
	pools, err := q.GetPolicyPools(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get pools: %w", err)
	}
	for _, r := range pools {
		spec.PoolIDs = append(spec.PoolIDs, r.PoolID)
	}
	creds, err := q.GetPolicyCredentials(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get credentials: %w", err)
	}
	for _, r := range creds {
		spec.CredentialIDs = append(spec.CredentialIDs, r.HostKeyID)
	}
	rls, err := q.GetPolicyRateLimits(ctx, id)
	if err != nil {
		return fmt.Errorf("policy.Get rateLimits: %w", err)
	}
	for _, r := range rls {
		link, err := rateLimitLinkFromRow(r.RateLimitID, r.Models)
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
		spec.BudgetIDs = append(spec.BudgetIDs, r.BudgetID)
	}
	return nil
}

// writeLinks replaces p's pool, credential, rate-limit and budget junction rows; q must be inside the Upsert transaction.
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
	if err := q.DeletePolicyCredentials(ctx, id); err != nil {
		return fmt.Errorf("clear credentials: %w", err)
	}
	for i, keyID := range p.Spec.CredentialIDs {
		if err := q.InsertPolicyCredential(ctx, gen.InsertPolicyCredentialParams{PolicyID: id, HostKeyID: keyID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert credential %s: %w", keyID, err)
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
		if err := q.InsertPolicyRateLimit(ctx, gen.InsertPolicyRateLimitParams{PolicyID: id, RateLimitID: l.RateLimitID, Models: models, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert rate limit %s: %w", l.RateLimitID, err)
		}
	}
	if err := q.DeletePolicyBudgets(ctx, id); err != nil {
		return fmt.Errorf("clear budgets: %w", err)
	}
	for i, budgetID := range p.Spec.BudgetIDs {
		if err := q.InsertPolicyBudget(ctx, gen.InsertPolicyBudgetParams{PolicyID: id, BudgetID: budgetID, Position: int32(i)}); err != nil {
			return fmt.Errorf("insert budget %s: %w", budgetID, err)
		}
	}
	return nil
}

func rateLimitLinkFromRow(rateLimitID string, models []byte) (RateLimitLink, error) {
	link := RateLimitLink{RateLimitID: rateLimitID}
	if err := json.Unmarshal(models, &link.Models); err != nil {
		return link, fmt.Errorf("models: %w", err)
	}
	// An unscoped link reads back as nil, as it was written.
	if len(link.Models) == 0 {
		link.Models = nil
	}
	return link, nil
}
