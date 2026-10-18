//go:build integration

// rewrite_integration_test.go covers moving rows between shapes: the background rewriter converting legacy rows, and migration 34's down refusing while sealed rows remain. Run with: make test-integration.

package stored_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/crypto"
)

// insertLegacy writes a row the way a relay from before the key ring did, past the trigger's refusal.
func insertLegacy(t *testing.T, pool *pgxpool.Pool, id string, plain []byte) {
	t.Helper()
	ct, nonce, err := crypto.Encrypt(keyA, plain)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL relay.secret_legacy_write = 'on'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO secret_values (id, ciphertext, nonce, key_version) VALUES ($1, $2, $3, 1)", id, ct, nonce); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Run, as the server starts it, converts legacy rows without being asked.
func TestIntegration_RunRewritesLegacyRows(t *testing.T) {
	pool := storagetest.Pool(t)
	insertLegacy(t, pool, "old", []byte("legacy"))
	b := backend(t, pool, keyA)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for rowOf(t, pool, "old").dek == "" {
		if time.Now().After(deadline) {
			t.Fatal("the legacy row was not rewritten")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := resolve(t, ctx, b, "old"); got != "legacy" || rowOf(t, pool, "old").scope != stored.ScopeSystem {
		t.Fatalf("rewritten row: %q in %q", got, rowOf(t, pool, "old").scope)
	}
}

// Migration 34's down refuses while values are sealed under data keys, even when run without the CLI's check.
func TestIntegration_SchemaRollbackRefusesSealedRows(t *testing.T) {
	dsn := storagetest.DB(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := backend(t, pool, keyA).CreateScoped(ctx, stored.ScopeSystem, "v", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n, err := backend(t, pool).SealedRows(ctx); err != nil || n != 1 {
		t.Fatalf("sealed rows: %d %v", n, err)
	}
	if err := storage.MigrateTo(dsn, 33); err == nil || !strings.Contains(err.Error(), "relay secret downgrade") {
		t.Fatalf("migration 34 down with a sealed row: %v", err)
	}
}
