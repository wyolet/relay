//go:build integration

// links_integration_test.go covers a Policy's links to pools, rate limits and budgets against a real Postgres: each lives in a junction, so order, replacement, the shared mark and what a deleted target leaves behind (nothing for a cap, its id for a pool) are the behaviour. Run with: make test-integration.

package policy_test

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/internal/storage/gen"
)

func TestIntegration_PolicyLinksRoundTrip(t *testing.T) {
	f, ctx := setupPolicyStore(t)
	pools := pool.NewStore(f.db)
	budgets := budget.NewStore(gen.New(f.db))

	var poolIDs []string
	for i := 0; i < 2; i++ {
		p := &pool.Pool{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "links-pool-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: pool.Spec{HostID: f.hostID, CredentialIDs: []string{f.hostKeyID}},
		}
		if err := pools.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert pool: %v", err)
		}
		poolIDs = append(poolIDs, p.Meta.ID)
	}
	b := &budget.Budget{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "links-budget-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: budget.Spec{AmountUSD: "100"},
	}
	if err := budgets.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert budget: %v", err)
	}

	want := policy.Spec{
		PoolIDs: []string{poolIDs[1], poolIDs[0]},
		RateLimits: []policy.RateLimitLink{
			{RateLimitID: f.rateLimitID},
			{RateLimitID: f.otherRateLimit, Models: []string{"acme/m"}, Shared: new(false)},
			{RateLimitID: f.otherRateLimit, Models: []string{"acme/n"}},
		},
		Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID, Shared: new(false)}},
	}
	p := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "links-policy-" + short(), Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: want,
	}
	if err := f.store.Upsert(ctx, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	assertLinks(t, "Get", got.Spec, want)
	if err := got.Validate(); err != nil {
		t.Errorf("the row read back does not validate: %v", err)
	}
	listed := listedPolicy(t, ctx, f.store, p.Meta.ID)
	assertLinks(t, "List", listed.Spec, want)

	// A rewrite replaces every link list rather than adding to it.
	p.Spec.PoolIDs = poolIDs[:1]
	p.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: f.otherRateLimit}}
	if err := f.store.Upsert(ctx, p); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, err = f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after rewrite: %v", err)
	}
	assertLinks(t, "rewrite", got.Spec, policy.Spec{
		PoolIDs:    poolIDs[:1],
		RateLimits: []policy.RateLimitLink{{RateLimitID: f.otherRateLimit}},
		Budgets:    want.Budgets,
	})

	// Deleting a linked budget or rate limit drops the link; a deleted pool stays linked by id, so the policy narrows instead of falling back to ownership.
	if err := pools.Delete(ctx, poolIDs[0]); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	if err := budgets.Delete(ctx, b.Meta.ID); err != nil {
		t.Fatalf("delete budget: %v", err)
	}
	if err := ratelimit.NewStore(gen.New(f.db)).Delete(ctx, f.otherRateLimit); err != nil {
		t.Fatalf("delete rate limit: %v", err)
	}
	got, err = f.store.Get(ctx, p.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after deletes: %v (row %v)", err, got)
	}
	assertLinks(t, "after deletes", got.Spec, policy.Spec{PoolIDs: poolIDs[:1]})
	assertLinks(t, "listed after deletes", listedPolicy(t, ctx, f.store, p.Meta.ID).Spec, policy.Spec{PoolIDs: poolIDs[:1]})
}

// assertLinks compares link lists with the shared mark read as its effective value: the store always returns it explicit.
func assertLinks(t *testing.T, step string, got, want policy.Spec) {
	t.Helper()
	if !slices.Equal(got.PoolIDs, want.PoolIDs) {
		t.Errorf("%s: poolIds = %v, want %v", step, got.PoolIDs, want.PoolIDs)
	}
	sameRate := slices.EqualFunc(got.RateLimits, want.RateLimits, func(a, b policy.RateLimitLink) bool {
		return a.RateLimitID == b.RateLimitID && slices.Equal(a.Models, b.Models) && a.IsShared() == b.IsShared() && a.Shared != nil
	})
	if !sameRate {
		t.Errorf("%s: rateLimits = %+v, want %+v", step, got.RateLimits, want.RateLimits)
	}
	sameBudget := slices.EqualFunc(got.Budgets, want.Budgets, func(a, b policy.BudgetLink) bool {
		return a.BudgetID == b.BudgetID && a.IsShared() == b.IsShared() && a.Shared != nil
	})
	if !sameBudget {
		t.Errorf("%s: budgets = %+v, want %+v", step, got.Budgets, want.Budgets)
	}
}

func listedPolicy(t *testing.T, ctx context.Context, s *policy.Store, id string) *policy.Policy {
	t.Helper()
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.Meta.ID == id {
			return r
		}
	}
	t.Fatalf("policy %s missing from List", id)
	return nil
}
