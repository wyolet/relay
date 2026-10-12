package main

import (
	"fmt"
	"log/slog"
	"strconv"

	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// runMigrate implements `relay migrate [up | down <version> | force <version> | auth down <version>]`.
// Up-migrations also run on boot; `up` applies them as a step of its own, so a rollout can migrate once before any pod starts. `down` is the rollback direction: an operator downgrading the image must first put the schema back where the old binary can read it. `force` is the recovery path after a migration failed half-way and left the schema dirty. `auth down` steps back only the auth module's migrations, which `down` leaves alone above relay's version 26.
func runMigrate(args []string) error {
	if len(args) == 0 {
		slog.Info("migrate: up-migrations run on boot; use 'relay migrate up' to run them now or 'relay migrate down <version>' to roll back")
		return nil
	}
	cmd := args[0]
	if cmd == "auth" {
		return runMigrateAuth(args[1:])
	}
	if cmd == "up" {
		if len(args) != 1 {
			return fmt.Errorf("migrate up: takes no arguments")
		}
		dsn, err := migrationDSN("up")
		if err != nil {
			return err
		}
		if err := storagemod.MigrateUp(dsn); err != nil {
			return err
		}
		slog.Info("migrate: schema is up to date")
		return nil
	}
	if cmd != "down" && cmd != "force" {
		return fmt.Errorf("migrate: unknown argument %q (want 'up', 'down <version>', 'force <version>' or 'auth down <version>')", cmd)
	}
	if len(args) != 2 {
		if cmd == "down" {
			return fmt.Errorf("migrate down: exactly one target version is required")
		}
		return fmt.Errorf("migrate force: exactly one version is required")
	}
	version, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return fmt.Errorf("migrate %s: %q is not a schema version", cmd, args[1])
	}

	dsn, err := migrationDSN(cmd)
	if err != nil {
		return err
	}
	if cmd == "force" {
		slog.Warn("migrate: recording the schema version and clearing the dirty flag; no migration runs",
			"version", version)
		if err := storagemod.ForceVersion(dsn, uint(version)); err != nil {
			return err
		}
		slog.Info("migrate: schema version recorded", "version", version)
		return nil
	}
	slog.Warn("migrate: running down-migrations; rows the newer schema owns are dropped",
		"target_version", version)
	if err := storagemod.MigrateTo(dsn, uint(version)); err != nil {
		return err
	}
	slog.Info("migrate: schema is at the requested version", "version", version)
	return nil
}

// runMigrateAuth implements `relay migrate auth down <version>`.
func runMigrateAuth(args []string) error {
	if len(args) != 2 || args[0] != "down" {
		return fmt.Errorf("migrate auth: want 'auth down <version>'")
	}
	version, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return fmt.Errorf("migrate auth down: %q is not a schema version", args[1])
	}
	// The module's version 1 is relay's own schema; going below it would drop the tables relay's migrations own.
	if version < 1 {
		return fmt.Errorf("migrate auth down: the lowest target is 1; 'relay migrate down' unwinds the tables themselves")
	}
	dsn, err := migrationDSN("auth down")
	if err != nil {
		return err
	}
	slog.Warn("migrate: running the auth module's down-migrations; the columns and tables they remove are dropped",
		"target_version", version)
	if err := authpg.MigrateDown(dsn, uint(version)); err != nil {
		return err
	}
	slog.Info("migrate: auth schema is at or below the requested version", "version", version)
	return nil
}

// migrationDSN loads the config and returns the DSN migrations run on, with its lock timeout applied.
func migrationDSN(cmd string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	if cfg.PGDSN == "" {
		return "", fmt.Errorf("RELAY_PG_DSN required for migrate %s", cmd)
	}
	return storagemod.MigrationDSN(cfg.PGDSN, cfg.MigrateLockTimeout)
}
