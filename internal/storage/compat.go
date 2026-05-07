package storage

// compat.go provides convenience constructors used by tests and the seed CLI.
// Production code should use Open() directly.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/catalog"
)

// OpenPool opens a raw pgxpool.Pool without running migrations.
// Intended for tests that manage the schema themselves.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn)
}

// PostgresFromPool wraps an existing pool into a *catalog.PGStore.
// Migrations must have been applied before calling.
// The caller owns the pool lifetime; the returned store must NOT be closed
// (Close() is a no-op since the pool is not owned by the store).
func PostgresFromPool(ctx context.Context, pool *pgxpool.Pool) (*catalog.PGStore, error) {
	st := WrapPool(pool)
	return catalog.NewPGStoreNoReload(st.Catalog, st)
}

// Postgres opens a pool, runs migrations, and returns a *catalog.PGStore.
// masterKey may be nil when stored-mode secrets are not in use.
func Postgres(ctx context.Context, dsn string, masterKey []byte) (*catalog.PGStore, error) {
	st, err := Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return catalog.NewPGStore(st.Catalog, st, masterKey)
}
