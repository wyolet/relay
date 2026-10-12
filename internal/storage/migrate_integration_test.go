//go:build integration

package storage_test

import (
	"context"
	"testing"

	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// setSchemaVersion opens dsn at the embedded max, then overwrites the
// golang-migrate bookkeeping row as a newer release would leave it.
func setSchemaVersion(t *testing.T, dsn string, version uint, dirty bool) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	defer st.Close()
	if _, err := st.Pool().Exec(ctx,
		`UPDATE schema_migrations SET version = $1, dirty = $2`, int64(version), dirty); err != nil {
		t.Fatalf("bump version: %v", err)
	}
}

func TestIntegration_Open_SchemaNewerThanBinary(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	setSchemaVersion(t, dsn, storagetest.LatestVersion(t)+5, false)

	st, err := storage.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open on newer schema: %v", err)
	}
	st.Close()
}

// The auth module's migrations run after the relay's, under their own version table.
func TestIntegration_Open_RunsAuthMigrations(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	var version int64
	if err := st.Pool().QueryRow(ctx, "SELECT version FROM "+authpg.MigrationsTable).Scan(&version); err != nil {
		t.Fatalf("auth schema version: %v", err)
	}
	if version < 1 {
		t.Fatalf("auth schema version %d, want at least 1", version)
	}
	if _, err := st.Pool().Exec(ctx, "UPDATE "+authpg.MigrationsTable+" SET version = version + 5"); err != nil {
		t.Fatal(err)
	}
	again, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open on an auth schema a newer binary migrated: %v", err)
	}
	again.Close()
}

func TestIntegration_Open_NewerSchemaDirtyFails(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	setSchemaVersion(t, dsn, storagetest.LatestVersion(t)+5, true)

	st, err := storage.Open(context.Background(), dsn)
	if err == nil {
		st.Close()
		t.Fatal("Open on dirty newer schema succeeded, want error")
	}
}
