package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
)

// DB is what a store runs its statements on: the pool, or a transaction InTx
// opened. Begin on a transaction opens a savepoint, so a store whose own
// write spans tables works unchanged inside a caller's transaction.
type DB interface {
	gen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// InTx runs fn in one repeatable-read transaction and commits when fn returns
// nil; an error or a panic rolls back every write fn made. Repeatable read
// keeps each row fn reads as it was when fn started, and fails a write to a
// row another transaction changed since rather than overwrite it.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx DB) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// SQLSTATE codes for a transaction a concurrent one got in the way of.
const (
	sqlstateSerializationFailure = "40001"
	sqlstateDeadlockDetected     = "40P01"
)

// IsConflict reports whether err is Postgres refusing a transaction because a
// concurrent one changed what it read (serialization failure) or locked what
// it needed (deadlock). Nothing was written; the same request may be retried.
func IsConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		(pgErr.Code == sqlstateSerializationFailure || pgErr.Code == sqlstateDeadlockDetected)
}
