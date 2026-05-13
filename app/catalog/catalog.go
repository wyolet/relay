package catalog

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// Catalog is the long-lived composition object. Holds the six entity stores
// and the current Snapshot pointer. Construct one per process; call Reload
// at boot and whenever PG state changes (admin write, NOTIFY watcher).
type Catalog struct {
	providers   ProviderLister
	policies    PolicyLister
	models      ModelLister
	keys        HostKeyLister
	rateLimits  RateLimitLister
	relayKeys   RelayKeyLister

	snap atomic.Pointer[Snapshot]
}

// Per-package narrow Lister interfaces — Catalog only needs List from each.
// Declared here on the consumer side so the concrete Store types satisfy
// them implicitly.

type ProviderLister interface {
	List(ctx context.Context) ([]*provider.Provider, error)
}
type PolicyLister interface {
	List(ctx context.Context) ([]*policy.Policy, error)
}
type ModelLister interface {
	List(ctx context.Context) ([]*model.Model, error)
}
type HostKeyLister interface {
	List(ctx context.Context) ([]*hostkey.HostKey, error)
}
type RateLimitLister interface {
	List(ctx context.Context) ([]*ratelimit.RateLimit, error)
}
type RelayKeyLister interface {
	List(ctx context.Context) ([]*relaykey.RelayKey, error)
}

// New constructs a Catalog backed by the supplied stores. Initial Snapshot
// is empty; call Reload before serving traffic.
func New(
	providers ProviderLister,
	policies PolicyLister,
	models ModelLister,
	keys HostKeyLister,
	rateLimits RateLimitLister,
	relayKeys RelayKeyLister,
) *Catalog {
	c := &Catalog{
		providers:  providers,
		policies:   policies,
		models:     models,
		keys:       keys,
		rateLimits: rateLimits,
		relayKeys:  relayKeys,
	}
	c.snap.Store(&Snapshot{})
	return c
}

// Current returns the live Snapshot. Safe to call from any goroutine; the
// returned pointer is immutable until the next successful Reload.
func (c *Catalog) Current() *Snapshot { return c.snap.Load() }

// Reload reads every store, filters to enabled rows, runs cross-entity
// validation, builds a fresh Snapshot, and atomic-swaps it in. On any
// error the existing Snapshot stays live — callers can retry.
func (c *Catalog) Reload(ctx context.Context) error {
	provs, err := c.providers.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: providers: %w", err)
	}
	pols, err := c.policies.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: policies: %w", err)
	}
	models, err := c.models.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: models: %w", err)
	}
	keys, err := c.keys.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: providerkeys: %w", err)
	}
	rls, err := c.rateLimits.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: ratelimits: %w", err)
	}
	rks, err := c.relayKeys.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: relaykeys: %w", err)
	}

	// Filter to enabled rows. Providers aren't kept in the Snapshot, but
	// their ids are needed for ownership validation and their slugs are
	// needed to compute the prefixed model name index.
	providerIDs := make(map[string]struct{}, len(provs))
	providerSlugByID := make(map[string]string, len(provs))
	for _, p := range provs {
		providerIDs[p.Meta.ID] = struct{}{}
		providerSlugByID[p.Meta.ID] = p.Meta.Name
	}
	enabledPols := filter(pols, (*policy.Policy).IsEnabled)
	enabledRKs := filter(rks, (*relaykey.RelayKey).IsEnabled)
	enabledModels := filter(models, (*model.Model).IsEnabled)
	enabledKeys := filter(keys, (*hostkey.HostKey).IsEnabled)
	enabledRLs := filter(rls, (*ratelimit.RateLimit).IsEnabled)

	if err := validateCross(providerIDs, enabledPols, enabledRKs, enabledModels, enabledKeys, enabledRLs); err != nil {
		return fmt.Errorf("catalog reload: %w", err)
	}

	snap := build(enabledPols, enabledRKs, enabledModels, enabledKeys, enabledRLs, providerSlugByID)
	c.snap.Store(snap)
	return nil
}

func filter[T any](items []T, keep func(T) bool) []T {
	out := items[:0]
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	// Detach so the input slice isn't aliased.
	cp := make([]T, len(out))
	copy(cp, out)
	return cp
}
