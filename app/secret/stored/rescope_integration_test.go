//go:build integration

// rescope_integration_test.go covers an owner change racing the background rewriter and the OAuth save-back: the value must end up in the new owner's scope. Run with: make test-integration.

package stored_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// insertHostKey adds the secrets row that owns the stored value at id.
func insertHostKey(t *testing.T, pool *pgxpool.Pool, id, ownerKind, ownerID string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO secrets (id, name, display_name, value_kind, metadata, spec)
		VALUES ($1, $1, $1, 'stored', jsonb_build_object('owner', jsonb_build_object('kind', $2::text, 'id', $3::text)), '{}')`,
		id, ownerKind, ownerID)
	if err != nil {
		t.Fatal(err)
	}
}

// changeOwner moves the host key to user:<ownerID> and rescopes its value in one transaction, as the host key store does, and returns it uncommitted.
func changeOwner(t *testing.T, pool *pgxpool.Pool, b *stored.Backend, id, ownerID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `UPDATE secrets SET metadata = jsonb_set(metadata, '{owner}', jsonb_build_object('kind', 'user', 'id', $2::text)) WHERE id = $1`, id, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WithDB(tx).Rescope(ctx, id, "user:"+ownerID); err != nil {
		t.Fatal(err)
	}
	return tx
}

// The rewriter lists a legacy row, the owner changes before it writes: the rescope converts the row under the new owner, and the rewrite finds it changed.
func TestIntegration_RescopeWinsOverARewriteOfALegacyRow(t *testing.T) {
	pool := storagetest.Pool(t)
	insertHostKey(t, pool, "hk", "user", "a")
	insertLegacy(t, pool, "hk", []byte("tok"))
	b := backend(t, pool, keyA)
	listed, err := b.StaleRows(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("stale rows: %v %v", listed, err)
	}
	if err := changeOwner(t, pool, b, "hk", "b").Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.RewriteRow(ctx, listed[0]); !errors.Is(err, stored.ErrValueChanged) {
		t.Fatalf("rewrite after the rescope: %v", err)
	}
	if got := rowOf(t, pool, "hk").scope; got != "user:b" {
		t.Fatalf("scope %q, want user:b", got)
	}
	if got := resolve(t, stored.WithScope(ctx, "user:b"), b, "hk"); got != "tok" {
		t.Fatalf("value under user:b: %q", got)
	}
}

// A process that cannot decrypt a legacy row leaves it on owner change; the rewriter that can reads the owner when it writes, not when it listed the row.
func TestIntegration_RewriteUsesTheOwnerAtWriteTime(t *testing.T) {
	pool := storagetest.Pool(t)
	insertHostKey(t, pool, "hk", "user", "a")
	insertLegacy(t, pool, "hk", []byte("tok"))
	b := backend(t, pool, keyA)
	listed, err := b.StaleRows(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("stale rows: %v %v", listed, err)
	}
	if err := changeOwner(t, pool, backend(t, pool, keyBad), "hk", "b").Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rowOf(t, pool, "hk"); got.dek != "" {
		t.Fatalf("a process without the master key converted the row: %+v", got)
	}
	if err := b.RewriteRow(ctx, listed[0]); err != nil {
		t.Fatal(err)
	}
	if got := rowOf(t, pool, "hk").scope; got != "user:b" {
		t.Fatalf("scope %q, want user:b", got)
	}
	if got := resolve(t, stored.WithScope(ctx, "user:b"), b, "hk"); got != "tok" {
		t.Fatalf("value under user:b: %q", got)
	}
}

// An OAuth save-back during an owner change waits for it and writes the new blob in the new scope.
func TestIntegration_SaveBackDuringRescopeLandsInTheNewScope(t *testing.T) {
	pool := storagetest.Pool(t)
	insertHostKey(t, pool, "oa", "user", "a")
	b := backend(t, pool, keyA)
	if _, err := b.CreateScoped(ctx, "user:a", "oa", []byte("blob-1")); err != nil {
		t.Fatal(err)
	}
	tx := changeOwner(t, pool, b, "oa", "b")
	done := make(chan error, 1)
	go func() {
		_, err := b.Create(ctx, "oa", []byte("blob-2"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the save-back did not wait for the owner change: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := rowOf(t, pool, "oa").scope; got != "user:b" {
		t.Fatalf("scope %q, want user:b", got)
	}
	if got := resolve(t, stored.WithScope(ctx, "user:b"), b, "oa"); got != "blob-2" {
		t.Fatalf("value under user:b: %q", got)
	}
}
