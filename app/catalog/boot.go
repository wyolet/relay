package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/internal/storage/gen"
)

// BootstrapOptions configures the one-call Bootstrap helper. Pool and
// MasterKey are required; MasterKey may be nil if stored-mode HostKeys
// aren't in use.
type BootstrapOptions struct {
	Pool      *pgxpool.Pool
	MasterKey []byte

	// AutoSeedDir, when non-empty AND the catalog is empty in PG, triggers
	// a YAML import from this directory before the initial Reload.
	// Idempotent: if any catalog row already exists, seeding is skipped.
	AutoSeedDir string
}

// Stores bundles the eight entity stores constructed by Bootstrap. Exposed
// so callers (admin handlers, seed CLI re-runs, tests) can reach the same
// underlying stores without re-wiring.
type Stores struct {
	Provider  *provider.Store
	Host      *host.Store
	Model     *model.Store
	HostKey   *hostkey.Store
	RateLimit *ratelimit.Store
	Policy    *policy.Store
	Pricing   *pricing.Store
	RelayKey  *relaykey.Store
}

// Bootstrap wires the eight entity stores against the pool, optionally
// seeds from YAML when the catalog is empty, runs an initial Reload, and
// constructs a Listener primed for Run(ctx). The Listener is not started
// — caller decides when to call Listener.Run (typically in a goroutine).
func Bootstrap(ctx context.Context, opts BootstrapOptions) (*Catalog, *Listener, *Stores, error) {
	if opts.Pool == nil {
		return nil, nil, nil, fmt.Errorf("catalog.Bootstrap: Pool is required")
	}
	q := gen.New(opts.Pool)
	stores := &Stores{
		Provider:  provider.NewStore(q),
		Host:      host.NewStore(q),
		Model:     model.NewStore(q),
		HostKey:   hostkey.NewStore(q, opts.MasterKey),
		RateLimit: ratelimit.NewStore(q),
		Policy:    policy.NewStore(opts.Pool),
		Pricing:   pricing.NewStore(opts.Pool),
		RelayKey:  relaykey.NewStore(q),
	}

	cat := New(
		stores.Provider, stores.Host, stores.Policy, stores.Model,
		stores.HostKey, stores.RateLimit, stores.RelayKey, stores.Pricing,
	)

	if opts.AutoSeedDir != "" {
		empty, err := isCatalogEmpty(ctx, stores)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("catalog.Bootstrap: check empty: %w", err)
		}
		if empty {
			if _, err := seed.Run(ctx, seed.Options{
				Pool:      opts.Pool,
				YAMLDir:   opts.AutoSeedDir,
				MasterKey: opts.MasterKey,
			}); err != nil {
				return nil, nil, nil, fmt.Errorf("catalog.Bootstrap: auto-seed: %w", err)
			}
		}
	}

	if err := cat.Reload(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("catalog.Bootstrap: initial reload: %w", err)
	}

	listener := NewListener(cat, opts.Pool, listenerStores{
		provider:  stores.Provider,
		host:      stores.Host,
		model:     stores.Model,
		hostkey:   stores.HostKey,
		ratelimit: stores.RateLimit,
		policy:    stores.Policy,
		pricing:   stores.Pricing,
		relaykey:  stores.RelayKey,
	})
	return cat, listener, stores, nil
}

// isCatalogEmpty returns true when every catalog table has zero rows.
// Cheap: just lists every store; bails on first non-empty result.
func isCatalogEmpty(ctx context.Context, s *Stores) (bool, error) {
	provs, err := s.Provider.List(ctx)
	if err != nil {
		return false, err
	}
	if len(provs) > 0 {
		return false, nil
	}
	hosts, err := s.Host.List(ctx)
	if err != nil {
		return false, err
	}
	if len(hosts) > 0 {
		return false, nil
	}
	models, err := s.Model.List(ctx)
	if err != nil {
		return false, err
	}
	if len(models) > 0 {
		return false, nil
	}
	keys, err := s.HostKey.List(ctx)
	if err != nil {
		return false, err
	}
	if len(keys) > 0 {
		return false, nil
	}
	rls, err := s.RateLimit.List(ctx)
	if err != nil {
		return false, err
	}
	if len(rls) > 0 {
		return false, nil
	}
	pols, err := s.Policy.List(ctx)
	if err != nil {
		return false, err
	}
	if len(pols) > 0 {
		return false, nil
	}
	prs, err := s.Pricing.List(ctx)
	if err != nil {
		return false, err
	}
	if len(prs) > 0 {
		return false, nil
	}
	rks, err := s.RelayKey.List(ctx)
	if err != nil {
		return false, err
	}
	return len(rks) == 0, nil
}
