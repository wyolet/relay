//go:build integration

package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// A row changed by another transaction after InTx read it fails InTx's write,
// and IsConflict recognises that failure.
func TestInTx_ConcurrentChangeIsConflict(t *testing.T) {
	pool := storagetest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE tx_rows (id int PRIMARY KEY, v int); INSERT INTO tx_rows VALUES (1, 0)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	err := storage.InTx(ctx, pool, func(ctx context.Context, tx storage.DB) error {
		var v int
		if err := tx.QueryRow(ctx, `SELECT v FROM tx_rows WHERE id = 1`).Scan(&v); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `UPDATE tx_rows SET v = 1 WHERE id = 1`); err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
		_, err := tx.Exec(ctx, `UPDATE tx_rows SET v = 2 WHERE id = 1`)
		return err
	})
	if !storage.IsConflict(err) {
		t.Fatalf("InTx error = %v, want a conflict", err)
	}
	if storage.IsConflict(errors.New("other")) || storage.IsConflict(nil) {
		t.Fatal("IsConflict matched an error that is not a conflict")
	}
	var v int
	if err := pool.QueryRow(ctx, `SELECT v FROM tx_rows WHERE id = 1`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("row = %d (err %v), want the concurrent write's 1", v, err)
	}
}
