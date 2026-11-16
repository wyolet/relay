//go:build integration

package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// The operator's system policies, rate limits and budgets move to the organization on any boot after the upgrade, whatever it seeds: a pinned release, a local tree into a catalog that is not empty, or nothing at all. A policy the earlier catalog seed marked detached moves too and loses the mark; the built-in rate limit stays system.
func TestIntegration_OrganizationMoveOnEveryBoot(t *testing.T) {
	rule := []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}}
	builtin := []*ratelimit.RateLimit{{Meta: meta.Metadata{Name: "inference-api", Owner: system}, Spec: ratelimit.Spec{Rules: rule}}}
	for _, tc := range []struct {
		name string
		opts func(t *testing.T, ctx context.Context, stores *Stores) BootstrapOptions
	}{
		{"versioned boot", func(t *testing.T, _ context.Context, _ *Stores) BootstrapOptions {
			return BootstrapOptions{CatalogVersion: "v9.9.9-test", AutoSeedDir: stampedTree(t, "v9.9.9-test"), CatalogURL: "http://127.0.0.1:1/{version}.tar.gz", SystemRateLimits: builtin}
		}},
		{"unpinned boot over a seeded catalog", func(t *testing.T, ctx context.Context, stores *Stores) BootstrapOptions {
			// A catalog already in place: the local tree is not seeded again.
			prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "seeded-earlier", Owner: system}}
			if err := stores.Provider.Upsert(ctx, prov); err != nil {
				t.Fatalf("earlier catalog row: %v", err)
			}
			return BootstrapOptions{AutoSeedDir: stampedTree(t, "v9.9.9-test"), SystemRateLimits: builtin}
		}},
		{"boot with no seed", func(*testing.T, context.Context, *Stores) BootstrapOptions {
			return BootstrapOptions{SystemRateLimits: builtin}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := storagetest.Pool(t)
			_, stores, err := BootstrapStores(ctx, BootstrapOptions{Pool: db})
			if err != nil {
				t.Fatalf("BootstrapStores: %v", err)
			}
			detachedAt := time.Now().UTC().Add(-time.Hour)
			ops := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-default", Owner: system}}
			marked := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-marked", Owner: system, CatalogDetachedAt: &detachedAt}}
			opsRL := &ratelimit.RateLimit{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-rpm", Owner: system}, Spec: ratelimit.Spec{Rules: rule}}
			bud := &budget.Budget{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-month", Owner: system}, Spec: budget.Spec{AmountUSD: "10", Period: budget.PeriodMonth, Mode: budget.ModeHard}}
			for _, err := range []error{
				stores.Policy.Upsert(ctx, ops), stores.Policy.Upsert(ctx, marked),
				stores.RateLimit.Upsert(ctx, opsRL), stores.Budget.Upsert(ctx, bud),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}

			opts := tc.opts(t, ctx, stores)
			opts.Pool = db
			cat, stores, err := BootstrapStores(ctx, opts)
			if err != nil {
				t.Fatalf("BootstrapStores: %v", err)
			}
			if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
			for _, row := range []struct{ table, id, want string }{
				{"policies", ops.Meta.ID, "organization"},
				{"policies", marked.Meta.ID, "organization"},
				{"rate_limits", opsRL.Meta.ID, "organization"},
				{"budgets", bud.Meta.ID, "organization"},
			} {
				if got := ownerKind(t, db, row.table, row.id); got != row.want {
					t.Errorf("%s %s owner = %q, want %q", row.table, row.id, got, row.want)
				}
			}
			for _, p := range []*policy.Policy{ops, marked} {
				if got, err := stores.Policy.Get(ctx, p.Meta.ID); err != nil || got.Meta.CatalogDetachedAt != nil {
					t.Errorf("%s = %+v, %v; want no detach mark", p.Meta.Name, got, err)
				}
			}
			var builtinOwner string
			if err := db.QueryRow(ctx, "SELECT metadata->'owner'->>'kind' FROM rate_limits WHERE name = 'inference-api'").Scan(&builtinOwner); err != nil || builtinOwner != "system" {
				t.Errorf("built-in rate limit owner = %q (%v), want system", builtinOwner, err)
			}
		})
	}
}
