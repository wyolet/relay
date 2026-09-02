//go:build integration

// missing_ids_integration_test.go covers the batched membership check
// against a real Postgres — the query has no fake seam. Run with:
// make test-integration.
package user_test

import (
	"context"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func setupUserDB(t *testing.T) (*user.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return user.NewStore(gen.New(pool)), ctx
}

func TestMissingIDsReportsOnlyAbsentUsers(t *testing.T) {
	store, ctx := setupUserDB(t)

	present := &user.User{ID: meta.NewID(), Username: "member-" + meta.NewID()[:8]}
	if err := store.Upsert(ctx, present); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, present.ID) })

	absent := meta.NewID()
	missing, err := store.MissingIDs(ctx, []string{present.ID, absent})
	if err != nil {
		t.Fatalf("MissingIDs: %v", err)
	}
	if len(missing) != 1 || missing[0] != absent {
		t.Fatalf("missing = %v, want [%s]", missing, absent)
	}

	if missing, err := store.MissingIDs(ctx, nil); err != nil || len(missing) != 0 {
		t.Fatalf("MissingIDs(nil) = (%v, %v), want (nil, nil)", missing, err)
	}
	if missing, err := store.MissingIDs(ctx, []string{present.ID}); err != nil || len(missing) != 0 {
		t.Fatalf("MissingIDs on an existing user = (%v, %v), want none missing", missing, err)
	}
}
