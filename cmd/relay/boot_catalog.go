package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/settings"
	relayconfig "github.com/wyolet/relay/config"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// catalogSeedLock is the advisory-lock id the catalog seed serializes on.
const catalogSeedLock int64 = 0x52454C41595F5345

func catalogBootOptions(cfg *config.Config, st *storagemod.Storage) appcatalog.BootstrapOptions {
	bootOpts := appcatalog.BootstrapOptions{
		Pool:      st.Pool(),
		MasterKey: cfg.MasterKey,
		SeedLock: func(ctx context.Context, fn func(context.Context) error) error {
			return storagemod.WithAdvisoryLock(ctx, st.Pool(), catalogSeedLock, fn)
		},
	}
	if cfg.AutoSeedIfEmpty && cfg.CatalogDir != "" {
		bootOpts.AutoSeedDir = cfg.CatalogDir
	}
	if cfg.CatalogVersion != "" {
		bootOpts.CatalogVersion = cfg.CatalogVersion
		bootOpts.CatalogURL = cfg.CatalogURL
		bootOpts.CatalogIndexURL = cfg.CatalogIndexURL
		slog.Info("catalog: version pinned", "version", cfg.CatalogVersion)
	}
	bootOpts.SystemRateLimits = loadSystemRateLimits(cfg)
	return bootOpts
}

// loadSystemRateLimits returns the system rate limit definitions the boot seeds: the ones shipped in the binary, each replaced by a definition of the same name under <config dir>/ratelimits when there is one.
func loadSystemRateLimits(cfg *config.Config) []*ratelimit.RateLimit {
	dir := filepath.Join(cfg.ConfigDir, "ratelimits")
	defs, err := seed.LoadSystemRateLimits(relayconfig.SystemRateLimits, dir)
	if err != nil {
		slog.Error("system rate limits invalid", "err", err, "dir", dir)
		os.Exit(1)
	}
	if len(defs.FromDir) > 0 {
		slog.Info("system rate limits: definitions taken from the config directory; they apply only to rows that do not exist yet",
			"dir", dir, "names", defs.FromDir)
	}
	if len(defs.Ignored) > 0 {
		slog.Warn("system rate limits: documents in the config directory that are not a built-in system rate limit were not loaded; use `relay apply` for them",
			"dir", dir, "documents", defs.Ignored)
	}
	return defs.Rows
}

func bootstrapCatalogStores(bootCtx context.Context, bootOpts appcatalog.BootstrapOptions) (*appcatalog.Catalog, *appcatalog.Stores) {
	// Stores-first: wire the catalog stores synchronously so the control
	// plane can serve CRUD even if the data-plane snapshot bootstrap
	// fails or stalls. Hydrate (seed + first Reload + NOTIFY listener)
	// runs in the background with retry — inference middleware gates
	// on catalog.IsReady() and returns 503 until the snapshot is built.
	cat, stores, err := appcatalog.BootstrapStores(bootCtx, bootOpts)
	if err != nil {
		slog.Error("catalog stores init failed", "err", err)
		os.Exit(1)
	}
	return cat, stores
}

func seedSettings(bootCtx context.Context, cfg *config.Config, stores *appcatalog.Stores) {
	// First-boot / airgapped settings seed: upsert any <section>.yaml from the
	// settings dir that has no DB row yet (seed-if-absent — never clobbers a
	// runtime change). Managed deployments configure at runtime via the
	// settings API instead; this just bootstraps a fresh instance. Runs before
	// hydrate so the seeded values land in the snapshot's first reload.
	settingsDir := os.Getenv("RELAY_SETTINGS_DIR")
	if settingsDir == "" {
		settingsDir = filepath.Join(cfg.ConfigDir, "settings")
	}
	if seeded, err := settings.SeedDir(bootCtx, stores.Settings, settingsDir); err != nil {
		slog.Error("settings seed failed", "err", err, "dir", settingsDir)
		os.Exit(1)
	} else if len(seeded) > 0 {
		slog.Info("settings: seeded from YAML", "dir", settingsDir, "sections", seeded)
	}
}

// hydrateLoop runs Catalog.Hydrate with exponential backoff until it
// succeeds, then starts the NOTIFY listener. Survives transient PG /
// seed errors without taking the process down; the data plane returns
// 503 until the first Hydrate completes. Once successful, the function
// blocks on Listener.Run until the parent context is cancelled.
func hydrateLoop(ctx context.Context, cat *appcatalog.Catalog, stores *appcatalog.Stores, opts appcatalog.BootstrapOptions) {
	delay := time.Second
	const maxDelay = 30 * time.Second
	for {
		listener, err := cat.Hydrate(ctx, stores, opts)
		if err == nil {
			slog.Info("catalog hydrated", "auto_seed_dir", opts.AutoSeedDir)
			if err := listener.Run(ctx); err != nil && err != context.Canceled {
				slog.Error("catalog listener exited", "err", err)
			}
			return
		}
		slog.Error("catalog hydrate failed; retrying", "err", err, "delay", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}
