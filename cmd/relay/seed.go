package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// seedDryRun parses every YAML doc in dir and reports the kind tally,
// failing on any parse error. No PG access.
func seedDryRun(dir string) error {
	docs, err := manifest.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("load yaml: %w", err)
	}
	counts := map[string]int{}
	for _, d := range docs {
		counts[d.Kind()]++
	}
	slog.Info("seed dry-run: parsed manifests", "from", dir, "counts", counts)
	return nil
}

// runSeed implements `relay seed --from <dir> [--apply]`. Without
// --apply the run is a dry-run: YAML is parsed and validated but no
// rows are written to PG. PG DSN and master key are read from the
// process environment via internal/config.
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	from := fs.String("from", "config", "Directory containing manifest YAMLs to seed from.")
	apply := fs.Bool("apply", false, "Write to PG. Omit for a dry-run that only parses + validates YAML.")
	clearDirty := fs.Bool("dirty", false, "Overwrite operator-edited ('dirty') rows too, resetting them to the catalog. Default skips them.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*apply {
		if err := seedDryRun(*from); err != nil {
			return err
		}
		slog.Info("seed dry-run ok (no changes written; pass --apply to commit)", "from", *from)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.PGDSN == "" {
		return fmt.Errorf("RELAY_PG_DSN required for seed --apply")
	}

	ctx := context.Background()
	st, err := storagemod.Open(ctx, cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("storage.Open: %w", err)
	}
	defer st.Close()

	result, err := seed.Run(ctx, seed.Options{
		Pool:       st.Pool(),
		YAMLDir:    *from,
		MasterKey:  cfg.MasterKey,
		ClearDirty: *clearDirty,
	})
	if err != nil {
		return err
	}
	slog.Info("seed applied",
		"from", *from,
		"providers", result.Providers,
		"hosts", result.Hosts,
		"models", result.Models,
		"host_keys", result.HostKeys,
		"rate_limits", result.RateLimits,
		"policies", result.Policies,
		"pricings", result.Pricings,
		"relay_keys", result.RelayKeys,
		"skipped_dirty", result.Skipped,
		"clear_dirty", *clearDirty,
	)
	return nil
}
