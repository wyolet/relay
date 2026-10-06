//go:build integration

package catalog

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/seed"
	relayconfig "github.com/wyolet/relay/config"
	"github.com/wyolet/relay/internal/storage"
)

func shippedSystemRateLimits(t *testing.T) []*ratelimit.RateLimit {
	t.Helper()
	defs, err := seed.LoadSystemRateLimits(relayconfig.SystemRateLimits, "")
	if err != nil {
		t.Fatal(err)
	}
	return defs.Rows
}

func bootWithSystemRateLimits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, extra func(*BootstrapOptions)) (*Catalog, *Stores) {
	t.Helper()
	opts := BootstrapOptions{Pool: pool, SystemRateLimits: shippedSystemRateLimits(t)}
	if extra != nil {
		extra(&opts)
	}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	return cat, stores
}

func storedRateLimits(t *testing.T, ctx context.Context, stores *Stores) map[string]*ratelimit.RateLimit {
	t.Helper()
	rows, err := stores.RateLimit.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ratelimit.RateLimit{}
	for _, r := range rows {
		if _, dup := out[r.Meta.Name]; dup {
			t.Fatalf("rate limit %q stored twice", r.Meta.Name)
		}
		out[r.Meta.Name] = r
	}
	return out
}

func requireAllSystemRateLimits(t *testing.T, ctx context.Context, stores *Stores) map[string]*ratelimit.RateLimit {
	t.Helper()
	got := storedRateLimits(t, ctx, stores)
	for _, def := range shippedSystemRateLimits(t) {
		row, ok := got[def.Meta.Name]
		if !ok {
			t.Fatalf("system rate limit %q missing; stored: %v", def.Meta.Name, sortedNames(got))
		}
		if row.Meta.Owner.Kind != meta.OwnerSystem {
			t.Errorf("%q owner = %q, want system", def.Meta.Name, row.Meta.Owner.Kind)
		}
	}
	return got
}

func sortedNames(m map[string]*ratelimit.RateLimit) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestIntegration_SystemRateLimitsOnFreshDatabase(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	cat, stores := bootWithSystemRateLimits(t, ctx, pool, nil)
	requireAllSystemRateLimits(t, ctx, stores)

	// Ready means the enabled rows are already in the snapshot the data plane reads.
	if !cat.IsReady() {
		t.Fatal("catalog not ready after hydrate")
	}
	if _, ok := cat.Current().SystemRateLimitByName("inference-api-proxy-anonymous"); !ok {
		t.Error("snapshot lacks inference-api-proxy-anonymous")
	}
	if _, ok := cat.Current().SystemRateLimitByName("control-api"); ok {
		t.Error("snapshot holds control-api, which ships disabled")
	}
}

func TestIntegration_SystemRateLimitEditsSurviveABoot(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	_, stores := bootWithSystemRateLimits(t, ctx, pool, nil)
	anon := storedRateLimits(t, ctx, stores)["inference-api-proxy-anonymous"]
	off := false
	anon.Spec.Enabled = &off
	anon.Meta.Dirty = true
	anon.Meta.ResourceVersion = ""
	if err := stores.RateLimit.Upsert(ctx, anon); err != nil {
		t.Fatal(err)
	}

	cat, stores := bootWithSystemRateLimits(t, ctx, pool, nil)
	got := requireAllSystemRateLimits(t, ctx, stores)["inference-api-proxy-anonymous"]
	if got.IsEnabled() || got.Meta.ID != anon.Meta.ID {
		t.Fatalf("after a second boot: enabled=%v id=%s, want the operator's disabled row %s", got.IsEnabled(), got.Meta.ID, anon.Meta.ID)
	}
	if _, ok := cat.Current().SystemRateLimitByName("inference-api-proxy-anonymous"); ok {
		t.Error("disabled row is in the snapshot")
	}
}

func TestIntegration_DeletedSystemRateLimitComesBack(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	_, stores := bootWithSystemRateLimits(t, ctx, pool, nil)
	gone := storedRateLimits(t, ctx, stores)["otlp-export"]
	if err := stores.RateLimit.Delete(ctx, gone.Meta.ID); err != nil {
		t.Fatal(err)
	}

	_, stores = bootWithSystemRateLimits(t, ctx, pool, nil)
	back := requireAllSystemRateLimits(t, ctx, stores)["otlp-export"]
	if back.Meta.ID == gone.Meta.ID || !back.IsEnabled() {
		t.Fatalf("otlp-export: id=%s enabled=%v, want a fresh row as shipped", back.Meta.ID, back.IsEnabled())
	}
}

func TestIntegration_SystemRateLimitsSurviveACatalogReseed(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	// Only system rate limits exist, so a later boot with a catalog still seeds it.
	_, stores := bootWithSystemRateLimits(t, ctx, pool, nil)
	first := requireAllSystemRateLimits(t, ctx, stores)

	for _, version := range []string{"v1.0.0-test", "v1.1.0-test"} {
		_, stores = bootWithSystemRateLimits(t, ctx, pool, func(o *BootstrapOptions) {
			o.AutoSeedDir = stampedTree(t, version)
			o.CatalogVersion = version
			o.CatalogURL = "http://127.0.0.1:1/{version}.tar.gz"
		})
		if v := catalogSourceVersion(t, stores, ctx); v != version {
			t.Fatalf("catalog-source = %q, want %q", v, version)
		}
		provs, err := stores.Provider.List(ctx)
		if err != nil || len(provs) != 1 {
			t.Fatalf("providers = %d err %v, want the catalog seeded", len(provs), err)
		}
		for name, row := range requireAllSystemRateLimits(t, ctx, stores) {
			if first[name] != nil && row.Meta.ID != first[name].Meta.ID {
				t.Errorf("%q was replaced by the catalog seed", name)
			}
		}
	}
}

func TestIntegration_ConcurrentBootsCreateSystemRateLimitsOnce(t *testing.T) {
	for _, locked := range []bool{true, false} {
		name := "unserialized"
		if locked {
			name = "seed lock"
		}
		t.Run(name, func(t *testing.T) {
			pool, ctx, cancel := setupDB(t)
			defer cancel()

			const pods = 4
			var wg sync.WaitGroup
			errs := make([]error, pods)
			for i := range pods {
				wg.Add(1)
				go func() {
					defer wg.Done()
					opts := BootstrapOptions{Pool: pool, SystemRateLimits: shippedSystemRateLimits(t)}
					if locked {
						opts.SeedLock = func(ctx context.Context, fn func(context.Context) error) error {
							return storage.WithAdvisoryLock(ctx, pool, catalogSeedLock, fn)
						}
					}
					cat, stores, err := BootstrapStores(ctx, opts)
					if err == nil {
						_, err = cat.Hydrate(ctx, stores, opts)
					}
					errs[i] = err
				}()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("pod %d: %v", i, err)
				}
			}
			_, stores, err := BootstrapStores(ctx, BootstrapOptions{Pool: pool})
			if err != nil {
				t.Fatal(err)
			}
			if got := requireAllSystemRateLimits(t, ctx, stores); len(got) != len(shippedSystemRateLimits(t)) {
				t.Fatalf("rate limits = %v", sortedNames(got))
			}
		})
	}
}
