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

// PoolOption tunes the connection pool. A zero/negative value is ignored, so
// callers can pass config knobs directly and unset ones keep the defaults.
type PoolOption func(*poolSettings)

type poolSettings struct {
	maxConns int32
	minConns int32
	migrate  bool
}

// WithMigrateOnBoot(false) stops Open from running the up-migrations. An
// operator rolling back runs `relay migrate down` and needs the pods that
// restart meanwhile to leave the schema where they put it.
func WithMigrateOnBoot(on bool) PoolOption {
	return func(s *poolSettings) { s.migrate = on }
}

// WithMaxConns overrides the pool's maximum connections (ignored if n <= 0).
func WithMaxConns(n int) PoolOption {
	return func(s *poolSettings) {
		if n > 0 {
			s.maxConns = int32(n)
		}
	}
}

// WithMinConns overrides the pool's warm-floor connections (ignored if n <= 0).
// A higher floor keeps connections pre-established, so bursty load never pays
// cold-connection (dial + DNS) latency on the request path.
func WithMinConns(n int) PoolOption {
	return func(s *poolSettings) {
		if n > 0 {
			s.minConns = int32(n)
		}
	}
}

// resolvePoolSettings applies the sizing defaults (MaxConns 10 / MinConns 2),
// then the caller's overrides, and clamps the warm floor to the ceiling so a
// misconfigured MinConns > MaxConns can't wedge pool creation.
func resolvePoolSettings(opts ...PoolOption) poolSettings {
	s := poolSettings{maxConns: 10, minConns: 2, migrate: true}
	for _, o := range opts {
		o(&s)
	}
	if s.minConns > s.maxConns {
		s.minConns = s.maxConns
	}
	return s
}

// Open opens a connection pool, runs pending migrations, and returns a
// ready-to-use *Storage. The returned Storage must be closed with Close
// when no longer needed. Pool sizing defaults to MaxConns 10 / MinConns 2;
// pass WithMaxConns / WithMinConns to override, WithMigrateOnBoot(false) to
// skip the migrations.
func Open(ctx context.Context, dsn string, opts ...PoolOption) (*Storage, error) {
	s := resolvePoolSettings(opts...)
	if s.migrate {
		if err := runMigrations(dsn); err != nil {
			return nil, fmt.Errorf("storage.Open: %w", err)
		}
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("storage.Open: parse DSN: %w", err)
	}

	cfg.MaxConns = s.maxConns
	cfg.MinConns = s.minConns
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

// WithAdvisoryLock runs fn while holding the transaction-scoped Postgres
// advisory lock named by key, so concurrent pods serialize on it. The lock
// is released when the transaction ends, including on a panic or a lost
// connection — nothing can leave it held.
func WithAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func(context.Context) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: advisory lock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("storage: advisory lock: %w", err)
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WrapPool wraps an existing *pgxpool.Pool into a *Storage without
// opening a new pool or running migrations. Intended for tests that
// supply their own pool. The returned Storage must NOT be closed —
// the caller owns the pool's lifetime.
func WrapPool(pool *pgxpool.Pool) *Storage { return &Storage{pool: pool} }
