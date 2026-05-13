// Package storage owns the Postgres connection pool and runs migrations.
//
// In the new arch the catalog "domain repos" that used to live here moved
// out: each `app/X.Store` constructs its own sqlc-backed queries via
// `gen.New(pool)` against the pool surfaced by Storage.Pool(). Storage's
// remaining job is composition-root plumbing — open, ping, hand out the
// pool, close.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Storage is the data-access handle the composition root passes around.
type Storage struct {
	pool *pgxpool.Pool
}

// Open opens a connection pool, runs pending migrations, and returns a
// ready-to-use *Storage. The returned Storage must be closed with Close
// when no longer needed.
func Open(ctx context.Context, dsn string) (*Storage, error) {
	if err := runMigrations(dsn); err != nil {
		return nil, fmt.Errorf("storage.Open: %w", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("storage.Open: parse DSN: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("storage.Open: open pool: %w", err)
	}

	return &Storage{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Storage) Close() { s.pool.Close() }

// Ping checks database connectivity.
func (s *Storage) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool returns the underlying pgxpool. Composition-root use only —
// domain code reaches Postgres via its own typed Store packages.
func (s *Storage) Pool() *pgxpool.Pool { return s.pool }

// WrapPool wraps an existing *pgxpool.Pool into a *Storage without
// opening a new pool or running migrations. Intended for tests that
// supply their own pool. The returned Storage must NOT be closed —
// the caller owns the pool's lifetime.
func WrapPool(pool *pgxpool.Pool) *Storage { return &Storage{pool: pool} }
