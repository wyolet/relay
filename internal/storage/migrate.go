package storage

import (
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

// runMigrations runs all pending up-migrations against dsn.
// It is a no-op when no new migrations exist.
func runMigrations(dsn string) error {
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("storage: open migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("storage: init migrations: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("storage: migrate up: %w", err)
	}
	return nil
}

// MigrateTo moves the schema to an exact version, running the down
// migrations when it is below the current one. Exported for the rollback
// path: an old binary cannot read rows a newer schema wrote, so downgrading
// the image means downgrading the schema first.
func MigrateTo(dsn string, version uint) error {
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("storage: open migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("storage: init migrations: %w", err)
	}
	defer m.Close()
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("storage: migrate to %d: %w", version, err)
	}
	return nil
}
