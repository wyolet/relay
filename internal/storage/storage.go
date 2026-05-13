// Package storage is the data-access tier for Wyolet Relay.
// It owns the Postgres connection policy, sqlc queries, transactions, migrations,
// and error translation. All other packages consume typed domain methods; none
// of them import pgx or sqlc-generated types.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
)

// Storage is the top-level data-access handle.
// Callers reach domain repos via the public fields (e.g. s.Catalog).
type Storage struct {
	// Catalog satisfies catalog.CatalogDB — use it to pass to catalog.NewPGStore.
	Catalog *catalogRepo

	policy *pgxpool.Pool
	db   gen.DBTX // policy or an in-progress pgx.Tx
}

// Open opens a connection policy, runs pending migrations, and returns a ready-to-use *Storage.
// The returned Storage must be closed with Close when no longer needed.
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

	policy, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("storage.Open: open policy: %w", err)
	}

	return newStorage(policy, policy), nil
}

// newStorage constructs a *Storage backed by the given executor (policy or tx).
func newStorage(policy *pgxpool.Pool, db gen.DBTX) *Storage {
	s := &Storage{policy: policy, db: db}
	s.Catalog = &catalogRepo{db: db}
	return s
}

// Close releases the connection policy. Must not be called on a tx-scoped Storage.
func (s *Storage) Close() {
	s.policy.Close()
}

// Ping checks database connectivity.
func (s *Storage) Ping(ctx context.Context) error {
	return s.policy.Ping(ctx)
}

// rawPool returns the underlying policy for same-package use only.
func (s *Storage) rawPool() *pgxpool.Pool { return s.policy }

// Pool returns the underlying pgxpool for callers that need direct access
// (e.g. app/catalog.Bootstrap, which constructs sqlc queries against the
// same pool). Composition-root use only — domain packages must not call
// this; they go through storage's typed methods.
func (s *Storage) Pool() *pgxpool.Pool { return s.policy }

// WrapPool wraps an existing *pgxpool.Pool into a *Storage without opening a new
// policy or running migrations. Intended for tests and the seed CLI that open their
// own policy via pgxpool directly. The returned Storage must NOT be closed (the
// caller owns the policy's lifetime).
func WrapPool(policy *pgxpool.Pool) *Storage {
	return newStorage(policy, policy)
}
