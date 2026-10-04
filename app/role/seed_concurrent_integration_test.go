//go:build integration

// seed_concurrent_integration_test.go runs SeedBuiltins from several goroutines
// against one Postgres, the way a multi-pod rollout starts. The serialising
// advisory lock is a real Postgres lock, so there is no fake seam: without
// it each boot mints its own id for the same missing role and the rows
// bindings point at stop existing. Run with: make test-integration.
package role_test

import (
	"context"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// roleSeedLock mirrors cmd/relay's builtinRoleSeedLock — the id every pod
// serialises the built-in role seed on.
const roleSeedLock int64 = 0x52454C41595F5242

// Pods booting together must converge on one row per built-in name, and a
// re-seed after that must not re-mint the ids RoleBindings point at.
func TestIntegration_ConcurrentSeedBuiltinsProducesEachRoleOnce(t *testing.T) {
	pool, ctx := storagetest.Pool(t), context.Background()
	store := role.NewStore(gen.New(pool))

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	// Start from a clean slate so the race is over inserts, not updates.
	existing, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range existing {
		if role.IsBuiltin(r.Meta.Name) {
			if err := store.Delete(ctx, r.Meta.ID); err != nil {
				t.Fatalf("clear %q: %v", r.Meta.Name, err)
			}
		}
	}

	lock := func(ctx context.Context, fn func(context.Context) error) error {
		return storage.WithAdvisoryLock(ctx, pool, roleSeedLock, fn)
	}

	const boots = 4
	errs := make([]error, boots)
	var wg sync.WaitGroup
	for i := 0; i < boots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = role.SeedBuiltins(ctx, store, nil, lock)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
	}

	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]string{} // name → id
	for _, r := range rows {
		if !role.IsBuiltin(r.Meta.Name) {
			continue
		}
		if prev, dup := seen[r.Meta.Name]; dup {
			t.Fatalf("role %q exists twice (%s and %s)", r.Meta.Name, prev, r.Meta.ID)
		}
		seen[r.Meta.Name] = r.Meta.ID
	}
	if len(seen) != len(builtins) {
		t.Fatalf("built-in roles = %d, want %d", len(seen), len(builtins))
	}

	// A later boot is a no-op: ids are what RoleBindings point at.
	if err := role.SeedBuiltins(ctx, store, nil, lock); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	rows, err = store.List(ctx)
	if err != nil {
		t.Fatalf("list after re-seed: %v", err)
	}
	for _, r := range rows {
		if id, ok := seen[r.Meta.Name]; ok && id != r.Meta.ID {
			t.Fatalf("role %q was re-minted: %s → %s", r.Meta.Name, id, r.Meta.ID)
		}
	}
}
