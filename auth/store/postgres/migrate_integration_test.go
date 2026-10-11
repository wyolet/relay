//go:build integration

package postgres

import (
	"context"
	"testing"
)

// setVersion records version as a newer binary would leave it.
func setVersion(t *testing.T, dsn string, version int64, dirty bool) {
	t.Helper()
	if _, err := openPool(t, dsn).Exec(context.Background(),
		"UPDATE "+MigrationsTable+" SET version = $1, dirty = $2", version, dirty); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateToleratesNewerSchema(t *testing.T) {
	dsn := migratedDB(t)
	setVersion(t, dsn, 99, false)
	if err := Migrate(dsn); err != nil {
		t.Fatalf("Migrate on a schema a newer binary migrated: %v", err)
	}
	var version int64
	if err := openPool(t, dsn).QueryRow(context.Background(), "SELECT version FROM "+MigrationsTable).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 99 {
		t.Fatalf("recorded version %d, want 99 left alone", version)
	}
}

func TestMigrateRefusesDirtyNewerSchema(t *testing.T) {
	dsn := migratedDB(t)
	setVersion(t, dsn, 99, true)
	if err := Migrate(dsn); err == nil {
		t.Fatal("Migrate on a dirty schema succeeded, want an error")
	}
}
