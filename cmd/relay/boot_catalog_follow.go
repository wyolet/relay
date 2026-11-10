package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/cataloghook"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/internal/config"
)

// buildCatalogFollower returns the follower of a relay on
// RELAY_CATALOG_VERSION=latest, nil for an exact pin or none.
func buildCatalogFollower(cfg *config.Config, stores *appcatalog.Stores, opts appcatalog.BootstrapOptions) *appcatalog.Follower {
	if !seed.IsLatestAlias(cfg.CatalogVersion) {
		if cfg.CatalogWebhookURL != "" {
			slog.Warn("catalog: RELAY_CATALOG_WEBHOOK_URL is ignored: release webhooks need RELAY_CATALOG_VERSION=latest")
		}
		return nil
	}
	return appcatalog.NewFollower(stores, opts)
}

// buildCatalogRegistrar returns the webhook registration of a following relay
// with RELAY_CATALOG_WEBHOOK_URL set, nil otherwise.
func buildCatalogRegistrar(cfg *config.Config, follower *appcatalog.Follower) *cataloghook.Registrar {
	if follower == nil || cfg.CatalogWebhookURL == "" {
		return nil
	}
	reg, err := cataloghook.NewRegistrar(cfg.CatalogIndexURL, cfg.CatalogWebhookURL, seed.Channel())
	if err != nil {
		slog.Error("catalog webhook config invalid", "err", err)
		os.Exit(1)
	}
	return reg
}

// runCatalogFollow starts the follower's checks and the webhook registration
// once the boot seed is done and the catalog is ready. Call it after the
// inference listener is serving, since registering triggers a challenge
// against its receiver.
func runCatalogFollow(ctx context.Context, cfg *config.Config, cat *appcatalog.Catalog, follower *appcatalog.Follower, reg *cataloghook.Registrar) {
	if follower == nil {
		return
	}
	go func() {
		if !waitCatalogReady(ctx, cat) {
			return
		}
		if reg != nil {
			go reg.Run(ctx)
		}
		follower.Run(ctx, cfg.CatalogPollInterval)
	}()
}

// catalogSync is the check POST /api/catalog/sync runs; nil when the relay
// doesn't follow the catalog.
func catalogSync(follower *appcatalog.Follower) func(context.Context) (appcatalog.CheckResult, error) {
	if follower == nil {
		return nil
	}
	return follower.Check
}

// waitCatalogReady reports whether the catalog became ready before ctx ended.
func waitCatalogReady(ctx context.Context, cat *appcatalog.Catalog) bool {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for !cat.IsReady() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}
