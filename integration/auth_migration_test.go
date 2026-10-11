//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	authpg "github.com/wyolet/relay/auth/store/postgres"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// Relay's down-migrations drop and its up-migrations recreate tables the auth module's later migrations extend, so a rollback below them steps the module back too and the next boot brings both schemas to head again.
func TestRollbackBelowSharedTablesRestoresAuthSchema(t *testing.T) {
	t.Parallel()
	for _, target := range []uint{25, 0} {
		dsn := storagetest.EmptyDB(t)
		boot := func() {
			st, err := storagemod.Open(context.Background(), dsn)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			st.Close()
		}
		boot()
		if v := authVersion(t, dsn); v != 2 {
			t.Fatalf("auth schema at %d after boot, want 2", v)
		}
		if err := storagemod.MigrateTo(dsn, target); err != nil {
			t.Fatalf("migrate down to %d: %v", target, err)
		}
		if v := authVersion(t, dsn); v != 1 {
			t.Fatalf("auth schema at %d after relay went down to %d, want 1", v, target)
		}
		boot()
		if v := authVersion(t, dsn); v != 2 {
			t.Fatalf("auth schema at %d after coming back up from %d, want 2", v, target)
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authpg.New(pool).Groups.List(context.Background()); err != nil {
			t.Errorf("groups after a round trip through %d: %v", target, err)
		}
		pool.Close()
	}
}

func authVersion(t *testing.T, dsn string) int64 {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var v int64
	if err := pool.QueryRow(context.Background(), "SELECT version FROM "+authpg.MigrationsTable).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", authpg.MigrationsTable, err)
	}
	return v
}
