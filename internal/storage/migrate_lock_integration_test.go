//go:build integration

package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// holdLock reads table in a transaction it keeps open, as a long-running one would; an ALTER TABLE then waits behind it. release ends the transaction.
func holdLock(t *testing.T, dsn, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Rollback(ctx)
			_ = conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

func schemaVersion(t *testing.T, dsn, table string) (int64, bool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	var version int64
	var dirty bool
	if err := conn.QueryRow(ctx, "SELECT version, dirty FROM "+table).Scan(&version, &dirty); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return version, dirty
}

// migrateBlocked runs migrate while the lock is held and requires it to give up with a lock timeout, promptly.
func migrateBlocked(t *testing.T, migrate func() error) {
	t.Helper()
	start := time.Now()
	err := migrate()
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("migration = %v, want a lock timeout", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the migration waited %s on a 1s lock timeout", took)
	}
}

// The module's 000004 alters users. Behind a transaction holding users it gives up instead of queueing every login behind it, leaves the schema clean, and runs once the transaction ends; its down-migration does the same.
func TestIntegration_AuthMigrationGivesUpOnALockedTable(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := authpg.MigrateDown(dsn, 3); err != nil {
		t.Fatalf("auth down to 3: %v", err)
	}
	migrationDSN, err := storage.MigrationDSN(dsn, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	release := holdLock(t, dsn, "users")
	migrateBlocked(t, func() error { return storage.MigrateUp(migrationDSN) })
	if v, dirty := schemaVersion(t, dsn, authpg.MigrationsTable); v != 3 || dirty {
		t.Fatalf("auth schema after the timeout = %d dirty=%v, want 3 clean", v, dirty)
	}
	release()
	if err := storage.MigrateUp(migrationDSN); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if v, dirty := schemaVersion(t, dsn, authpg.MigrationsTable); v != 4 || dirty {
		t.Fatalf("auth schema after the retry = %d dirty=%v, want 4 clean", v, dirty)
	}

	release = holdLock(t, dsn, "users")
	migrateBlocked(t, func() error { return authpg.MigrateDown(migrationDSN, 3) })
	if v, dirty := schemaVersion(t, dsn, authpg.MigrationsTable); v != 4 || dirty {
		t.Fatalf("auth schema after the down timeout = %d dirty=%v, want 4 clean", v, dirty)
	}
	release()
	if err := authpg.MigrateDown(migrationDSN, 3); err != nil {
		t.Fatalf("down retry: %v", err)
	}
}

// Relay's 000032 alters providers; its runner takes the same timeout.
func TestIntegration_RelayMigrationGivesUpOnALockedTable(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := storage.MigrateTo(dsn, 31); err != nil {
		t.Fatalf("migrate down to 31: %v", err)
	}
	migrationDSN, err := storage.MigrationDSN(dsn, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	release := holdLock(t, dsn, "providers")
	migrateBlocked(t, func() error { return storage.MigrateUp(migrationDSN) })
	if v, dirty := schemaVersion(t, dsn, "schema_migrations"); v != 31 || dirty {
		t.Fatalf("schema after the timeout = %d dirty=%v, want 31 clean", v, dirty)
	}
	release()
	if err := storage.MigrateUp(migrationDSN); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if v, dirty := schemaVersion(t, dsn, "schema_migrations"); v != 32 || dirty {
		t.Fatalf("schema after the retry = %d dirty=%v, want 32 clean", v, dirty)
	}
}
