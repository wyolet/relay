//go:build integration

package postgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// 000004 alters users. Behind a transaction holding users, a lock_timeout in the dsn makes it give up, the version stays clean, and the next Migrate runs it.
func TestMigrateGivesUpOnALockedTableAndRetries(t *testing.T) {
	ctx := context.Background()
	dsn := migratedDB(t)
	if err := MigrateDown(dsn, 3); err != nil {
		t.Fatalf("MigrateDown to 3: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("lock_timeout", "500")
	u.RawQuery = q.Encode()
	bounded := u.String()

	tx, err := openPool(t, dsn).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM users LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(bounded); err == nil || !strings.Contains(err.Error(), "lock timeout") {
		_ = tx.Rollback(ctx)
		t.Fatalf("Migrate behind a held lock = %v, want a lock timeout", err)
	}
	var version int64
	var dirty bool
	if err := openPool(t, dsn).QueryRow(ctx, "SELECT version, dirty FROM "+MigrationsTable).Scan(&version, &dirty); err != nil {
		t.Fatal(err)
	}
	if version != 3 || dirty {
		t.Fatalf("schema after the timeout = %d dirty=%v, want 3 clean", version, dirty)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(bounded); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
