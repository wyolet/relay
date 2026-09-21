package main

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// runMigrate implements `relay migrate [down <version>]`. Up-migrations run
// implicitly on boot; only the rollback direction needs a command, because
// an operator downgrading the image must first put the schema back where the
// old binary can read it.
func runMigrate(args []string) error {
	if len(args) == 0 {
		slog.Info("migrate: up-migrations run on boot; use 'relay migrate down <version>' to roll back")
		return nil
	}
	if args[0] != "down" {
		return fmt.Errorf("migrate: unknown argument %q (want 'down <version>')", args[0])
	}
	if len(args) != 2 {
		return fmt.Errorf("migrate down: exactly one target version is required")
	}
	version, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return fmt.Errorf("migrate down: %q is not a schema version", args[1])
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.PGDSN == "" {
		return fmt.Errorf("RELAY_PG_DSN required for migrate down")
	}
	slog.Warn("migrate: running down-migrations; rows the newer schema owns are dropped",
		"target_version", version)
	if err := storagemod.MigrateTo(cfg.PGDSN, uint(version)); err != nil {
		return err
	}
	slog.Info("migrate: schema is at the requested version", "version", version)
	return nil
}
