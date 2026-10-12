//go:build integration

package main

import (
	"context"
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
