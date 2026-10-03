package storage

import (
	"errors"
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

// ForceVersion records version as the schema's current one and clears the
// dirty flag, running no migration. A migration that failed half-way leaves
// the schema dirty and every later migrate refuses it; the operator repairs
// the schema by hand, then records where it now stands.
func ForceVersion(dsn string, version uint) error {
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("storage: open migration source: %w", err)
	}
	// A version with no migration would strand every later migrate on
	// "no migration found".
	up, _, err := src.ReadUp(version)
	if err != nil {
		return fmt.Errorf("storage: force %d: no migration with that version: %w", version, err)
	}
	_ = up.Close()
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("storage: init migrations: %w", err)
	}
	defer m.Close()
	if err := m.Force(int(version)); err != nil {
		return fmt.Errorf("storage: force %d: %w", version, err)
	}
	return nil
}

// downAll is the target that means "unwind every migration".
const downAll = 0

// migrateDownTarget validates a `migrate down` target against the schema's
// current version. hasVersion is false on a database no migration has run
// against yet.
func migrateDownTarget(current uint, hasVersion bool, target uint) (downAllRun bool, err error) {
	if hasVersion && target > current {
		return false, fmt.Errorf(
			"storage: migrate down to %d: the schema is at %d — a higher target would migrate UP; "+
				"pass a version at or below the current one", target, current)
	}
	return target == downAll, nil
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
	current, _, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	downAllRun, err := migrateDownTarget(current, err == nil, version)
	if err != nil {
		return err
	}
	// Migrate(0) is not a valid target for golang-migrate; unwinding
	// everything is its own call.
	if downAllRun {
		if err := m.Down(); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("storage: migrate down to 0: %w", err)
		}
		return nil
	}
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("storage: migrate to %d: %w", version, err)
	}
	return nil
}
