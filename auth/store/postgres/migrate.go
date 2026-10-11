package postgres

import (
	"embed"
	"errors"
	"fmt"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// MigrationsTable is where the module records its schema version. It is its own table so the module's numbering never collides with the product's migrations on a shared database.
const MigrationsTable = "auth_schema_migrations"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate runs the module's pending up-migrations against dsn, a postgres:// or postgresql:// URL. Running it when nothing is pending does nothing.
func Migrate(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("auth migrations: up: %w", err)
	}
	return nil
}

func newMigrate(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("auth migrations: open source: %w", err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return nil, fmt.Errorf("auth migrations: init: %w", err)
	}
	return m, nil
}

// migrateURL points dsn at golang-migrate's pgx driver and at MigrationsTable.
func migrateURL(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("auth migrations: parse dsn: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
	default:
		return "", fmt.Errorf("auth migrations: dsn scheme %q: want postgres:// or postgresql://", u.Scheme)
	}
	q := u.Query()
	q.Set("x-migrations-table", MigrationsTable)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
