package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
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

	// CatalogVersion, when non-empty, pins the seeded catalog to a
	// published relay-catalog ref (tag), or — as "latest"/"auto" —
	// resolves the newest release for this binary's schema channel via
	// the channel index (CatalogIndexURL) at every hydrate. Once
	// concrete, the stored "catalog-source" marker is compared against
	// it; on mismatch (or an empty catalog) the tree is seeded — from
	// AutoSeedDir when its .version stamp matches (no network), else
	// fetched from CatalogURL — and the marker updated. The seed is
	// layering-safe: operator-edited (dirty) rows are skipped and
	// overlays re-merge at snapshot load. A resolve/fetch failure
	// against a non-empty catalog logs and continues with the existing
	// rows (never blocks boot); against an empty catalog it falls back to
	// AutoSeedDir when set, else fails hydrate (retried by the caller).
	CatalogVersion string

	// CatalogURL overrides the archive URL template used by
	// CatalogVersion fetches ("{version}" substituted). Empty uses
	// seed.DefaultCatalogURLTemplate (the wyolet/relay-catalog GitHub
	// archive). Point it at a mirror for airgapped deployments.
	CatalogURL string

	// CatalogIndexURL overrides where "latest"/"auto" resolves the
	// channel index from. Empty uses seed.DefaultCatalogIndexURL.
	CatalogIndexURL string
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
	Key       *key.Store
	Overlay   *overlay.Store
	Settings  *settings.Store
	Team      *team.Store
	Project   *project.Store

	ServiceAccount *serviceaccount.Store
	Group          *group.Store
	Role           *role.Store
	RoleBinding    *rolebinding.Store
	PolicyBinding  *policybinding.Store

	// Secrets is the shared secret-resolution registry (env + stored
	// backends). Exposed so data-plane components (e.g. the payload-logging
	// controller resolving S3 credentials) resolve through the same seam.
	Secrets *pkgsecret.Registry

	// Stored is the AES-GCM stored-secret backend registered in Secrets,
	// exposed so the composition root can write a secret (the generated
	// token signing key) through the same master-key path.
	Stored *pkgsecret.StoredResolver

	// OAuthResolver is the KindOAuth resolver registered in Secrets,
	// exposed so the composition root can drive the proactive
	// pkgoauth.Refresher against the same instance (shared single-flight).
	OAuthResolver *pkgoauth.Resolver
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
		Key:       key.NewStore(q),
		Overlay:   overlay.NewStore(q),
		Settings:  settings.NewStore(q),
		Team:      team.NewStore(q),
		Project:   project.NewStore(q),

		ServiceAccount: serviceaccount.NewStore(q),
		Group:          group.NewStore(opts.Pool),
		Role:           role.NewStore(q),
		RoleBinding:    rolebinding.NewStore(opts.Pool),
		PolicyBinding:  policybinding.NewStore(opts.Pool),

		Secrets: secReg,
		Stored:  secStored,
	}
	cat := New(
		stores.Provider, stores.Host, stores.Policy, stores.Model,
		stores.HostKey, stores.RateLimit, stores.Key, stores.Pricing,
		stores.Binding,
	)
	cat.UseOverlays(stores.Overlay)
	cat.UseTenancy(stores.Team, stores.Project, stores.ServiceAccount, stores.Group,
		stores.Role, stores.RoleBinding, stores.PolicyBinding)
	cat.settings.store = stores.Settings

	// OAuth credential resolver: stores its token blob via the same AES-GCM
	// path as KindStored, and refreshes on expiry using the live
	// oauth:<provider> settings section. Registered here (not in secret.Wire)
	// because the provider-config lookup reads the catalog settings cache,
	// which only exists once cat is built. Refresh is off the hot path (load /
	// post-401 heal), so the cache (populated by Hydrate before any resolve) is
	// always ready by the time a token actually needs refreshing.
	oauthResolver := pkgoauth.NewResolver(secStored,
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
		})
	secReg.Register(pkgsecret.KindOAuth, oauthResolver)
	stores.OAuthResolver = oauthResolver

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
	if opts.CatalogVersion != "" {
		if err := seedVersioned(ctx, stores, opts); err != nil {
			return nil, fmt.Errorf("catalog.Hydrate: %w", err)
		}
	} else if opts.AutoSeedDir != "" {
		empty, err := isCatalogEmpty(ctx, stores)
		if err != nil {
			return nil, fmt.Errorf("catalog.Hydrate: check empty: %w", err)
		}
		if empty {
			if _, err := seed.Run(ctx, seed.Options{
				Pool:             opts.Pool,
				YAMLDir:          opts.AutoSeedDir,
				MasterKey:        opts.MasterKey,
				CatalogKindsOnly: true,
			}); err != nil {
				return nil, fmt.Errorf("catalog.Hydrate: auto-seed: %w", err)
			}
			// A stamped tree (baked image) makes the seeded version known;
			// record it so a later matching version pin no-ops.
			if v := seed.DirVersion(opts.AutoSeedDir); v != "" {
				if err := writeCatalogSource(ctx, stores, v); err != nil {
					return nil, fmt.Errorf("catalog.Hydrate: %w", err)
				}
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
		key:       stores.Key,
		overlay:   stores.Overlay,
		settings:  stores.Settings,
		team:      stores.Team,
		project:   stores.Project,

		serviceAccount: stores.ServiceAccount,
		group:          stores.Group,
		role:           stores.Role,
		roleBinding:    stores.RoleBinding,
		policyBinding:  stores.PolicyBinding,
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

// seedVersioned reconciles the seeded catalog with opts.CatalogVersion:
// resolve "latest"/"auto" to a concrete release via the channel index,
// then seed + marker update when the stored catalog-source marker
// disagrees (or the catalog is empty), no-op otherwise. A version that
// matches the local tree's .version stamp seeds from disk without a
// fetch. See the BootstrapOptions.CatalogVersion doc for the failure
// policy.
func seedVersioned(ctx context.Context, stores *Stores, opts BootstrapOptions) error {
	row, err := stores.Settings.Get(ctx, settings.SectionCatalogSource)
	if err != nil {
		return fmt.Errorf("read catalog-source marker: %w", err)
	}
	cur, _ := row.Value.(*settings.CatalogSource)
	empty, err := isCatalogEmpty(ctx, stores)
	if err != nil {
		return fmt.Errorf("check empty: %w", err)
	}

	version := opts.CatalogVersion
	if seed.IsLatestAlias(version) {
		resolved, err := seed.ResolveLatest(ctx, opts.CatalogIndexURL, seed.Channel())
		if err != nil {
			if !empty {
				slog.Error("catalog: latest resolution failed; keeping existing catalog",
					"channel", seed.Channel(), "err", err)
				return nil
			}
			return seedLocalFallback(ctx, stores, opts, err)
		}
		version = resolved
	}

	if cur != nil && cur.Version == version && !empty {
		return nil
	}

	// The baked/local tree already holds this exact release — seed from
	// disk, no network.
	if v := seed.DirVersion(opts.AutoSeedDir); v != "" && v == version {
		return seedAndMark(ctx, stores, opts, opts.AutoSeedDir, version, cur, "local")
	}

	tmp, err := os.MkdirTemp("", "relay-catalog-*")
	if err != nil {
		return fmt.Errorf("catalog fetch tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	dataDir, fetchErr := seed.FetchCatalog(ctx, opts.CatalogURL, version, tmp)
	if fetchErr != nil {
		if !empty {
			// Availability over freshness: the existing rows keep serving;
			// the mismatch is retried on the next boot.
			slog.Error("catalog: versioned fetch failed; keeping existing catalog",
				"version", version, "err", fetchErr)
			return nil
		}
		if opts.AutoSeedDir != "" {
			return seedLocalFallback(ctx, stores, opts, fetchErr)
		}
		return fmt.Errorf("fetch catalog %s: %w", version, fetchErr)
	}
	return seedAndMark(ctx, stores, opts, dataDir, version, cur, "fetched")
}

// seedLocalFallback seeds AutoSeedDir after a resolve/fetch failure on an
// empty catalog. The marker is written only when the tree is stamped —
// an unstamped tree's version stays unknown, so the fetch is retried
// until it succeeds.
func seedLocalFallback(ctx context.Context, stores *Stores, opts BootstrapOptions, cause error) error {
	if opts.AutoSeedDir == "" {
		return fmt.Errorf("resolve catalog %s: %w", opts.CatalogVersion, cause)
	}
	slog.Error("catalog: versioned fetch failed on empty catalog; seeding local dir instead",
		"version", opts.CatalogVersion, "dir", opts.AutoSeedDir, "err", cause)
	if _, err := seed.Run(ctx, seed.Options{
		Pool: opts.Pool, YAMLDir: opts.AutoSeedDir, MasterKey: opts.MasterKey,
		CatalogKindsOnly: true,
	}); err != nil {
		return fmt.Errorf("fallback seed: %w", err)
	}
	if v := seed.DirVersion(opts.AutoSeedDir); v != "" {
		return writeCatalogSource(ctx, stores, v)
	}
	return nil
}

// seedAndMark runs the seed from dataDir and records version in the
// catalog-source marker.
func seedAndMark(ctx context.Context, stores *Stores, opts BootstrapOptions, dataDir, version string, cur *settings.CatalogSource, source string) error {
	res, err := seed.Run(ctx, seed.Options{
		Pool: opts.Pool, YAMLDir: dataDir, MasterKey: opts.MasterKey,
		CatalogKindsOnly: true,
	})
	if err != nil {
		return fmt.Errorf("seed catalog %s: %w", version, err)
	}
	if err := writeCatalogSource(ctx, stores, version); err != nil {
		return err
	}
	prev := ""
	if cur != nil {
		prev = cur.Version
	}
	slog.Info("catalog: seeded version",
		"version", version, "previous", prev, "source", source,
		"models", res.Models, "bindings", res.HostBindings, "pricings", res.Pricings,
		"skipped_dirty", res.Skipped)
	return nil
}

func writeCatalogSource(ctx context.Context, stores *Stores, version string) error {
	marker, err := json.Marshal(settings.CatalogSource{
		Version:  version,
		SeededAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal catalog-source marker: %w", err)
	}
	if _, err := stores.Settings.Upsert(ctx, settings.SectionCatalogSource, marker); err != nil {
		return fmt.Errorf("write catalog-source marker: %w", err)
	}
	return nil
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
	rks, err := s.Key.List(ctx)
	if err != nil {
		return false, err
	}
	return len(rks) == 0, nil
}
