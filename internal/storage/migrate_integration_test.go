//go:build integration

package storage

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

// freshDB creates a dedicated database on the RELAY_TEST_PG_DSN server and
// returns its DSN; it is dropped on cleanup.
func freshDB(t *testing.T, name string) string {
	t.Helper()
	adminDSN := os.Getenv("RELAY_TEST_PG_DSN")
	if adminDSN == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop db: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// setSchemaVersion opens dsn at the embedded max, then overwrites the
// golang-migrate bookkeeping row as a newer release would leave it.
func setSchemaVersion(t *testing.T, dsn string, version uint, dirty bool) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	defer st.Close()
	if _, err := st.Pool().Exec(ctx,
		`UPDATE schema_migrations SET version = $1, dirty = $2`, int64(version), dirty); err != nil {
		t.Fatalf("bump version: %v", err)
	}
}

func embeddedMax(t *testing.T) uint {
	t.Helper()
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	v, err := lastVersion(src)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIntegration_Open_SchemaNewerThanBinary(t *testing.T) {
	dsn := freshDB(t, "relay_newer_schema")
	setSchemaVersion(t, dsn, embeddedMax(t)+5, false)

	st, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open on newer schema: %v", err)
	}
	st.Close()
}

func TestIntegration_Open_NewerSchemaDirtyFails(t *testing.T) {
	dsn := freshDB(t, "relay_newer_schema_dirty")
	setSchemaVersion(t, dsn, embeddedMax(t)+5, true)

	st, err := Open(context.Background(), dsn)
	if err == nil {
		st.Close()
		t.Fatal("Open on dirty newer schema succeeded, want error")
	}
}
