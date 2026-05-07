//go:build integration

package storage_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/wyolet/relay/internal/catalog"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// startWatcherPG spins up a testcontainer Postgres with migrations applied and
// returns the DSN. It is defined here (not in testhelpers) to stay self-contained
// within the watcher test file.
func startWatcherPG(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("relay_test"),
		tcpostgres.WithUsername("relay"),
		tcpostgres.WithPassword("relay"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start pg: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	return dsn
}

// TestCatalogWatcher_NotifyFanout verifies the end-to-end NOTIFY/LISTEN path:
// a write through storage A's catalog triggers a reload on storage B via the watcher.
func TestCatalogWatcher_NotifyFanout(t *testing.T) {
	ctx := context.Background()

	// One Postgres instance shared by both "pods".
	dsn := startWatcherPG(t)

	// Storage A — the writer pod. MustOpenStorageMigrated runs migrations.
	stA := storagemod.MustOpenStorageMigrated(ctx, t, dsn)
	pgStoreA, err := catalog.NewPGStoreNoReload(stA.Catalog, stA)
	if err != nil {
		t.Fatalf("pgStoreA: %v", err)
	}
	t.Cleanup(pgStoreA.Close)
	if err := pgStoreA.Reload(ctx); err != nil {
		t.Fatalf("pgStoreA reload: %v", err)
	}

	// Storage B — the reader pod. Shares same DB but independent snapshot.
	stB := storagemod.MustOpenStorageMigrated(ctx, t, dsn)
	pgStoreB, err := catalog.NewPGStoreNoReload(stB.Catalog, stB)
	if err != nil {
		t.Fatalf("pgStoreB: %v", err)
	}
	t.Cleanup(pgStoreB.Close)
	if err := pgStoreB.Reload(ctx); err != nil {
		t.Fatalf("pgStoreB reload: %v", err)
	}

	// Start the catalog watcher on B — it will call pgStoreB.Reload on NOTIFY.
	watcher, err := storagemod.NewCatalogWatcher(ctx, dsn, func() {
		if err := pgStoreB.Reload(ctx); err != nil {
			t.Logf("watcher reload error: %v", err)
		}
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewCatalogWatcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	// Confirm the provider is not yet on B.
	if _, found := pgStoreB.ProviderByName("watcher-test-prov"); found {
		t.Fatal("expected provider to be absent on B before write")
	}

	// Write a provider through A's storage layer — this emits NOTIFY relay_catalog.
	prov := catalog.Provider{
		APIVersion: catalog.APIVersion,
		Kind:       catalog.KindProvider,
		Metadata:   catalog.Metadata{Name: "watcher-test-prov"},
		Spec:       catalog.ProviderSpec{Kind: catalog.PKOllama, BaseURL: "http://localhost:11434"},
	}
	if err := stA.Catalog.UpsertProvider(ctx, prov); err != nil {
		t.Fatalf("UpsertProvider on A: %v", err)
	}

	// Poll B's snapshot for up to 2 seconds; the watcher should fire and reload.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := pgStoreB.ProviderByName("watcher-test-prov"); found {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout: provider did not appear on B's snapshot within 2s after NOTIFY")
}
