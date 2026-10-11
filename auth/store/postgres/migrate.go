package postgres

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// MigrationsTable is where the module records its schema version. It is its own table so the module's numbering never collides with the product's migrations on a shared database.
const MigrationsTable = "auth_schema_migrations"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationFiles returns the embedded migration files, for a product that fingerprints its schema, such as to key a cached test database.
func MigrationFiles() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded, so Sub cannot fail
	}
	return sub
}

// Migrate runs the module's pending up-migrations against dsn, a postgres:// or postgresql:// URL. Running it when nothing is pending does nothing. A clean schema at a version newer than this binary embeds is left alone, so an older binary still boots during a rolling upgrade or a rollback; that relies on the module's migrations staying backward-compatible.
func Migrate(dsn string) error {
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("auth migrations: open source: %w", err)
	}
	embeddedMax, err := lastVersion(src)
	if err != nil {
		return fmt.Errorf("auth migrations: read source: %w", err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return fmt.Errorf("auth migrations: init: %w", err)
	}
	defer m.Close()
	current, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("auth migrations: read schema version: %w", err)
	}
	if err == nil && !dirty && current > embeddedMax {
		slog.Warn("auth migrations: schema is newer than this binary; skipping", "db_version", current, "embedded_max", embeddedMax)
		return nil
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("auth migrations: up: %w", err)
	}
	return nil
}

// MigrateDown runs the module's down-migrations until the schema is at version; a schema at or below it, or never migrated, is left alone. A product whose own down-migrations drop tables that the module's later migrations change calls it first, so the next Migrate applies them again.
func MigrateDown(dsn string, version uint) error {
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("auth migrations: open source: %w", err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return fmt.Errorf("auth migrations: init: %w", err)
	}
	defer m.Close()
	current, _, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth migrations: read schema version: %w", err)
	}
	if current <= version {
		return nil
	}
	if err := m.Migrate(version); err != nil {
		return fmt.Errorf("auth migrations: down to %d: %w", version, err)
	}
	return nil
}

// lastVersion returns the highest migration version src holds.
func lastVersion(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, err
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, err
		}
		v = next
	}
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
