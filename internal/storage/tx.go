package storage

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/internal/catalog"
)

// WithTx runs fn inside a Postgres transaction.
// A new *Storage whose repos are bound to that transaction is passed to fn.
// The outer policy is preserved so that Close() still works on the outer Storage.
//
// Commits when fn returns nil; rolls back otherwise.
// Domain code never sees pgx.Tx — it just uses the same *Storage API.
// WithTxCatalog implements catalog.TxRunner.
// It opens a transaction and calls fn with a CatalogDB backed by that transaction.
func (s *Storage) WithTxCatalog(ctx context.Context, fn func(db catalog.CatalogDB) error) error {
	pgTx, err := s.policy.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage.WithTxCatalog: begin: %w", err)
	}
	txRepo := &catalogRepo{db: pgTx}
	if err := fn(txRepo); err != nil {
		_ = pgTx.Rollback(ctx)
		return err
	}
	if err := pgTx.Commit(ctx); err != nil {
		_ = pgTx.Rollback(ctx)
		return fmt.Errorf("storage.WithTxCatalog: commit: %w", err)
	}
	return nil
}

func (s *Storage) WithTx(ctx context.Context, fn func(tx *Storage) error) error {
	pgTx, err := s.policy.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage.WithTx: begin: %w", err)
	}
	txStorage := newStorage(s.policy, pgTx)
	if err := fn(txStorage); err != nil {
		_ = pgTx.Rollback(ctx)
		return err
	}
	if err := pgTx.Commit(ctx); err != nil {
		_ = pgTx.Rollback(ctx)
		return fmt.Errorf("storage.WithTx: commit: %w", err)
	}
	return nil
}
