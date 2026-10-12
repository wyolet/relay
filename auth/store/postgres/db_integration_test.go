//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// envDSN names the server the test databases are created on; each test gets a database of its own, dropped on cleanup.
const envDSN = "RELAY_TEST_PG_DSN"

// productMigrations is the migration tree of the product that shares these tables in this repository; tests that need it fail when it is absent, so a moved tree cannot turn them into passes that tested nothing.
var productMigrations = filepath.Join("..", "..", "..", "migrations", "postgres")

func emptyDB(t *testing.T) string {
	t.Helper()
	server := os.Getenv(envDSN)
	if server == "" {
		t.Skip(envDSN + " not set; run via `make test-integration`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "auth_t_" + hex.EncodeToString(b)
	execOn(t, ctx, server, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		execOn(t, ctx, server, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("parse %s: %v", envDSN, err)
	}
	u.Path = "/" + name
	return u.String()
}

func execOn(t *testing.T, ctx context.Context, dsn, stmt string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// migratedDB returns a database holding only the module's schema.
func migratedDB(t *testing.T) string {
	t.Helper()
	dsn := emptyDB(t)
	if err := Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrateProduct applies the product's own migrations under its default version table.
func migrateProduct(t *testing.T, dsn string) {
	t.Helper()
	if _, err := os.Stat(productMigrations); err != nil {
		t.Fatalf("product migrations not found at %s: %v", productMigrations, err)
	}
	src, err := iofs.New(os.DirFS(productMigrations), ".")
	if err != nil {
		t.Fatal(err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	q := u.Query()
	q.Del("x-migrations-table")
	u.RawQuery = q.Encode()
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("product migrations: %v", err)
	}
}
