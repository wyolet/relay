//go:build integration

package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wyolet/relay/internal/storage"
)

// largeTree writes a seedable tree big enough that two unserialized seeds
// overlap: each plans against an empty catalog, then both insert the same
// names under different ids.
func largeTree(t *testing.T, providers int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < providers; i++ {
		name := fmt.Sprintf("acme-%03d", i)
		pdir := filepath.Join(dir, "providers", name)
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		yaml := fmt.Sprintf("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata:\n    name: %s\nspec:\n    homepageURL: https://%s.test\n", name, name)
		if err := os.WriteFile(filepath.Join(pdir, "provider.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// catalogSeedLock mirrors cmd/relay's catalogSeedLock.
const catalogSeedLock int64 = 0x52454C41595F5345

// Pods booting together against an empty database must seed the catalog
// once, not race each other into duplicate-name failures.
func TestIntegration_ConcurrentFirstBootsSeedOnce(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	const pods, providers = 4, 150
	opts := BootstrapOptions{
		Pool:        pool,
		AutoSeedDir: largeTree(t, providers),
		SeedLock: func(ctx context.Context, fn func(context.Context) error) error {
			return storage.WithAdvisoryLock(ctx, pool, catalogSeedLock, fn)
		},
	}
	var wg sync.WaitGroup
	errs := make([]error, pods)
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cat, stores, err := BootstrapStores(ctx, opts)
			if err == nil {
				_, err = cat.Hydrate(ctx, stores, opts)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("pod %d: hydrate failed: %v", i, err)
		}
	}

	_, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	provs, err := stores.Provider.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != providers {
		t.Fatalf("providers = %d, want %d", len(provs), providers)
	}
}
