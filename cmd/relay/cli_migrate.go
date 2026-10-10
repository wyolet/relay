package main

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// runMigrate implements `relay migrate [up | down <version> | force <version>]`.
// Up-migrations also run on boot; `up` applies them as a step of its own, so a rollout can migrate once before any pod starts. `down` is the rollback direction: an operator downgrading the image must first put the schema back where the old binary can read it. `force` is the recovery path after a migration failed half-way and left the schema dirty.
func runMigrate(args []string) error {
	if len(args) == 0 {
		slog.Info("migrate: up-migrations run on boot; use 'relay migrate up' to run them now or 'relay migrate down <version>' to roll back")
		return nil
	}
	cmd := args[0]
	if cmd == "up" {
		if len(args) != 1 {
			return fmt.Errorf("migrate up: takes no arguments")
		}
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if cfg.PGDSN == "" {
			return fmt.Errorf("RELAY_PG_DSN required for migrate up")
		}
		if err := storagemod.MigrateUp(cfg.PGDSN); err != nil {
			return err
		}
		slog.Info("migrate: schema is up to date")
		return nil
	}
	if cmd != "down" && cmd != "force" {
		return fmt.Errorf("migrate: unknown argument %q (want 'up', 'down <version>' or 'force <version>')", cmd)
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

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.PGDSN == "" {
		return fmt.Errorf("RELAY_PG_DSN required for migrate %s", cmd)
	}
	if cmd == "force" {
		slog.Warn("migrate: recording the schema version and clearing the dirty flag; no migration runs",
			"version", version)
		if err := storagemod.ForceVersion(cfg.PGDSN, uint(version)); err != nil {
			return err
		}
		slog.Info("migrate: schema version recorded", "version", version)
		return nil
	}
	slog.Warn("migrate: running down-migrations; rows the newer schema owns are dropped",
		"target_version", version)
	if err := storagemod.MigrateTo(cfg.PGDSN, uint(version)); err != nil {
		return err
	}
	slog.Info("migrate: schema is at the requested version", "version", version)
	return nil
}
