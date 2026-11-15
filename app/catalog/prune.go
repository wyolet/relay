package catalog

// prune.go runs catalogprune over the catalog stores: after a versioned
// reseed, under the seed lock that reseed holds, on a follower's check that
// seeds nothing, and on demand for the control API.

import (
	"context"
	"log/slog"
	"time"

	"github.com/wyolet/relay/app/catalogprune"
	"github.com/wyolet/relay/app/catalogstore"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
)

// DefaultPruneIdle is how long a detached model or host binding must have
// served no request before a prune deletes it.
const DefaultPruneIdle = 30 * 24 * time.Hour

// PruneOptions configures the prune of detached catalog rows.
type PruneOptions struct {
	// Auto prunes after every versioned reseed and on a follower's checks.
	Auto bool
	// Idle is the traffic window; 0 uses DefaultPruneIdle.
	Idle time.Duration
	// References lists the rows referencing a catalog row (an API singular
	// kind) as the control API's delete sees them. Nil fails every prune.
	References func(ctx context.Context, stores *Stores, kind, id string) ([]catalogprune.Ref, error)
	// Usage answers the traffic guard; nil keeps every detached model and
	// host binding.
	Usage catalogprune.UsageReader
	// UsageReady reports whether Usage can answer yet; the follower's prune
	// between releases waits for it. Nil counts a set Usage as ready.
	UsageReady func() bool
}

func (p PruneOptions) usageReady() bool {
	return p.Usage != nil && (p.UsageReady == nil || p.UsageReady())
}

func (p PruneOptions) idle(override time.Duration) time.Duration {
	switch {
	case override > 0:
		return override
	case p.Idle > 0:
		return p.Idle
	}
	return DefaultPruneIdle
}

// PruneDetached judges every detached catalog row and, with apply, deletes the
// ones nothing holds, under the seed lock so no reseed runs meanwhile; it
// answers ErrSeedBusy when another relay holds that lock. idle 0 uses the
// configured window.
func PruneDetached(ctx context.Context, stores *Stores, opts BootstrapOptions, idle time.Duration, apply bool) (catalogprune.Report, error) {
	if !apply {
		return runPrune(ctx, stores, opts.Prune, idle, false)
	}
	var rep catalogprune.Report
	run := func(ctx context.Context) error {
		var err error
		rep, err = runPrune(ctx, stores, opts.Prune, idle, true)
		return err
	}
	if opts.SeedTryLock == nil {
		return rep, run(ctx)
	}
	ran, err := opts.SeedTryLock(ctx, run)
	if err == nil && !ran {
		err = ErrSeedBusy
	}
	return rep, err
}

func runPrune(ctx context.Context, stores *Stores, p PruneOptions, idle time.Duration, apply bool) (catalogprune.Report, error) {
	o := catalogprune.Options{Idle: p.idle(idle), Apply: apply, Usage: p.Usage}
	if p.References != nil {
		o.References = func(ctx context.Context, kind, id string) ([]catalogprune.Ref, error) {
			return p.References(ctx, stores, kind, id)
		}
	}
	return catalogprune.Run(ctx, catalogprune.Stores{
		Providers: stores.Provider, Hosts: stores.Host, Models: stores.Model, Pricings: stores.Pricing,
		Bindings: stores.Binding, Policies: stores.Policy, RateLimits: stores.RateLimit, Overlays: stores.Overlay,
	}, o)
}

// pruneAfterSeed is the automatic prune at the end of a versioned reseed; nil
// when it is off or failed, which leaves the detached rows for the next one.
func pruneAfterSeed(ctx context.Context, stores *Stores, p PruneOptions, version string) *settings.CatalogPrune {
	if !p.Auto {
		return nil
	}
	rep, err := runPrune(ctx, stores, p, 0, true)
	if err != nil {
		slog.Error("catalog: prune after the seed failed; detached rows stay for the next one",
			"version", version, "err", err)
		return nil
	}
	return &settings.CatalogPrune{Pruned: rep.Pruned, KeptDetached: rep.Kept}
}

// pruneBacklog is the prune a follower's check runs when it seeds nothing, so
// rows an earlier release detached don't wait for the next release. It skips,
// at the cost of one query, when the prune is off, usage can't answer yet or
// no row is detached. The caller holds the seed lock.
func pruneBacklog(ctx context.Context, stores *Stores, p PruneOptions) *settings.CatalogPrune {
	if !p.Auto || !p.usageReady() || stores.pool == nil {
		return nil
	}
	detached, err := catalogstore.NewDetachedStore(gen.New(stores.pool)).Any(ctx)
	if err != nil {
		slog.Warn("catalog: checking for detached rows failed; pruning on the next check", "err", err)
		return nil
	}
	if !detached {
		return nil
	}
	rep, err := runPrune(ctx, stores, p, 0, true)
	if err != nil {
		slog.Error("catalog: prune of detached rows failed; retrying on the next check", "err", err)
		return nil
	}
	if rep.Pruned > 0 {
		slog.Info("catalog: pruned detached rows", "pruned", rep.Pruned, "kept_detached", rep.Kept)
	}
	return &settings.CatalogPrune{Pruned: rep.Pruned, KeptDetached: rep.Kept}
}
