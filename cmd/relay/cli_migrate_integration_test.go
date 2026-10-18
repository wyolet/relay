//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

func versionIn(t *testing.T, dsn, table string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	var v int64
	if err := conn.QueryRow(ctx, "SELECT version FROM "+table).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return v
}

// A legacy-bytes row without a nonce fails migration 34's down, so the CLI refuses it before golang-migrate marks the schema dirty.
func TestIntegration_MigrateDownRefusesARowWithoutANonce(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.DB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL relay.secret_legacy_write = 'on'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO secret_values (id, ciphertext, nonce, key_version) VALUES ('odd', '\\x01', NULL, 1)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before := versionIn(t, dsn, "schema_migrations")
	if err := runMigrate([]string{"down", "33"}); err == nil || !strings.Contains(err.Error(), "relay secret downgrade") {
		t.Fatalf("migrate down 33 with a nonce-less row: %v", err)
	}
	var dirty bool
	if err := conn.QueryRow(ctx, "SELECT dirty FROM schema_migrations").Scan(&dirty); err != nil || dirty {
		t.Fatalf("schema dirty after the refusal: %v %v", dirty, err)
	}
	if got := versionIn(t, dsn, "schema_migrations"); got != before {
		t.Fatalf("schema moved from %d to %d", before, got)
	}
}

// `relay migrate down` above relay's version 26 leaves the auth module alone; `relay migrate auth down` steps back only the module, and the next up reapplies it.
func TestIntegration_MigrateAuthDownStepsBackOnlyTheModule(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	if err := runMigrate([]string{"up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	relayBefore, authBefore := versionIn(t, dsn, "schema_migrations"), versionIn(t, dsn, authpg.MigrationsTable)
	if authBefore <= 1 {
		t.Fatalf("auth schema at %d, want past its first migration", authBefore)
	}

	if err := runMigrate([]string{"auth", "down", "1"}); err != nil {
		t.Fatalf("migrate auth down 1: %v", err)
	}
	if got := versionIn(t, dsn, authpg.MigrationsTable); got != 1 {
		t.Fatalf("auth schema = %d, want 1", got)
	}
	if got := versionIn(t, dsn, "schema_migrations"); got != relayBefore {
		t.Fatalf("relay schema moved from %d to %d", relayBefore, got)
	}

	if err := runMigrate([]string{"up"}); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if got := versionIn(t, dsn, authpg.MigrationsTable); got != authBefore {
		t.Fatalf("auth schema after up = %d, want %d", got, authBefore)
	}
}
