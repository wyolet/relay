package storage

// compat.go provides convenience constructors used by tests and the seed CLI.
// Production code should use Open() directly.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/catalog"
)

// openPool opens a raw pgxpool.Pool without running migrations.
// Package-internal; used by MustOpenStorage and MustOpenPool.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn)
}

// Postgres opens a policy, runs migrations, and returns a *catalog.PGStore.
// masterKey may be nil when stored-mode secrets are not in use.
func Postgres(ctx context.Context, dsn string, masterKey []byte) (*catalog.PGStore, error) {
	st, err := Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return catalog.NewPGStore(st.Catalog, st, masterKey)
}
