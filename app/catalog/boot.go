package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
)

// BootstrapOptions configures the one-call Bootstrap helper. Pool and
// MasterKey are required; MasterKey may be nil if stored-mode HostKeys
// aren't in use.
type BootstrapOptions struct {
	Pool      *pgxpool.Pool
	MasterKey []byte

	// AutoSeedDir, when non-empty AND the catalog is empty in PG, triggers
	// a YAML import from this directory before the initial Reload. The
	// expected layout matches wyolet/relay-catalog's data/ tree (providers/
	// <provider>/{provider.yaml,models/}, hosts/<host>/{host.yaml,pricing/,
	// policies/}). filepath.WalkDir walks the tree; dispatch is by the
	// kind field in each YAML doc, so the nested layout is transparent.
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
	Binding   *binding.Store
	RelayKey  *relaykey.Store
	Overlay   *overlay.Store
	Settings  *settings.Store

	// Secrets is the shared secret-resolution registry (env + stored
	// backends). Exposed so data-plane components (e.g. the payload-logging
	// controller resolving S3 credentials) resolve through the same seam.
	Secrets *pkgsecret.Registry
}

// BootstrapStores wires the eight entity stores against the pool and
// constructs a Catalog. Does NOT touch row data — no seed, no Reload.
// Use when the control plane needs the stores but data-plane readiness
// is deferred (see (*Catalog).Hydrate). Cheap and rarely fails.
func BootstrapStores(ctx context.Context, opts BootstrapOptions) (*Catalog, *Stores, error) {
	if opts.Pool == nil {
		return nil, nil, fmt.Errorf("catalog.BootstrapStores: Pool is required")
	}
	q := gen.New(opts.Pool)
	secReg, secStored := appsecret.Wire(q, opts.Pool, opts.MasterKey)
	stores := &Stores{
		Provider:  provider.NewStore(q),
		Host:      host.NewStore(q),
		Model:     model.NewStore(q),
		HostKey:   hostkey.NewStore(q, secReg, secStored),
		RateLimit: ratelimit.NewStore(q),
		Policy:    policy.NewStore(opts.Pool),
		Pricing:   pricing.NewStore(opts.Pool),
		Binding:   binding.NewStore(opts.Pool),
		RelayKey:  relaykey.NewStore(q),
		Overlay:   overlay.NewStore(q),
		Settings:  settings.NewStore(q),
		Secrets:   secReg,
	}
	cat := New(
		stores.Provider, stores.Host, stores.Policy, stores.Model,
		stores.HostKey, stores.RateLimit, stores.RelayKey, stores.Pricing,
		stores.Binding,
	)
	cat.UseOverlays(stores.Overlay)
	cat.settings.store = stores.Settings

	// OAuth credential resolver: stores its token blob via the same AES-GCM
	// path as KindStored, and refreshes on expiry using the live
	// oauth:<provider> settings section. Registered here (not in secret.Wire)
	// because the provider-config lookup reads the catalog settings cache,
	// which only exists once cat is built. Refresh is off the hot path (load /
	// post-401 heal), so the cache (populated by Hydrate before any resolve) is
	// always ready by the time a token actually needs refreshing.
	secReg.Register(pkgsecret.KindOAuth, pkgoauth.NewResolver(secStored,
		func(provider string) (sdkoauth.ProviderConfig, bool) {
			v, ok := cat.Setting(settings.OAuthSection(provider))
			if !ok {
				return sdkoauth.ProviderConfig{}, false
			}
			pc, ok := v.(*settings.OAuthProvider)
			if !ok || pc == nil {
				return sdkoauth.ProviderConfig{}, false
			}
			return pc.ProviderConfig, true
		}))

	return cat, stores, nil
}

// Hydrate is the expensive half of bootstrap: reload settings, load the
// hostkey master-key version, optionally auto-seed from YAML, run the
// first catalog Reload, and construct a NOTIFY listener primed for Run.
// On any error the Catalog's IsReady stays false and the caller can
// retry — handlers gate on it and return 503 in the meantime.
func (c *Catalog) Hydrate(ctx context.Context, stores *Stores, opts BootstrapOptions) (*Listener, error) {
	if err := c.settings.reload(ctx); err != nil {
		return nil, fmt.Errorf("catalog.Hydrate: settings reload: %w", err)
	}
	if err := stores.HostKey.LoadKeyVersion(ctx); err != nil {
		return nil, fmt.Errorf("catalog.Hydrate: load key version: %w", err)
	}
	if opts.AutoSeedDir != "" {
		empty, err := isCatalogEmpty(ctx, stores)
		if err != nil {
			return nil, fmt.Errorf("catalog.Hydrate: check empty: %w", err)
		}
		if empty {
			if _, err := seed.Run(ctx, seed.Options{
				Pool:      opts.Pool,
				YAMLDir:   opts.AutoSeedDir,
				MasterKey: opts.MasterKey,
			}); err != nil {
				return nil, fmt.Errorf("catalog.Hydrate: auto-seed: %w", err)
			}
		}
	}
	if err := c.Reload(ctx); err != nil {
		return nil, fmt.Errorf("catalog.Hydrate: initial reload: %w", err)
	}
	listener := NewListener(c, opts.Pool, listenerStores{
		provider:  stores.Provider,
		host:      stores.Host,
		model:     stores.Model,
		hostkey:   stores.HostKey,
		ratelimit: stores.RateLimit,
		policy:    stores.Policy,
		pricing:   stores.Pricing,
		relaykey:  stores.RelayKey,
		overlay:   stores.Overlay,
		settings:  stores.Settings,
	})
	return listener, nil
}

// Bootstrap is the legacy one-shot: stores + Hydrate in a single call.
// Kept for tests and any caller that doesn't need split-boot semantics.
// Returns the same triple as before plus the listener primed for Run.
func Bootstrap(ctx context.Context, opts BootstrapOptions) (*Catalog, *Listener, *Stores, error) {
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := cat.Hydrate(ctx, stores, opts)
	if err != nil {
		return nil, nil, nil, err
	}
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
