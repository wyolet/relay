package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source"
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
	embeddedMax, err := lastVersion(src)
	if err != nil {
		return fmt.Errorf("storage: read migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("storage: init migrations: %w", err)
	}
	defer m.Close()

	// A newer release may have migrated this DB (rolling upgrade or rollback).
	// Boot on it instead of failing on a version this binary has no file for;
	// that relies on migrations staying backward-compatible.
	current, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	if err == nil && !dirty && current > embeddedMax {
		slog.Warn("storage: schema is newer than this binary; skipping migrations",
			"db_version", current, "embedded_max", embeddedMax)
		return nil
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("storage: migrate up: %w", err)
	}
	return nil
}

// lastVersion walks src and returns the highest migration version it holds.
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
