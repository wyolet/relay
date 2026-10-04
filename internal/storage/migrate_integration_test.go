//go:build integration

package storage_test

import (
	"context"
	"testing"

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

func TestIntegration_Open_NewerSchemaDirtyFails(t *testing.T) {
	dsn := storagetest.EmptyDB(t)
	setSchemaVersion(t, dsn, storagetest.LatestVersion(t)+5, true)

	st, err := storage.Open(context.Background(), dsn)
	if err == nil {
		st.Close()
		t.Fatal("Open on dirty newer schema succeeded, want error")
	}
}
