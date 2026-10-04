package catalog

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

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
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// Catalog is the long-lived composition object. Holds the entity stores
// and the current Snapshot pointer. Construct one per process; call Reload
// at boot and whenever PG state changes (admin write, NOTIFY watcher).
type Catalog struct {
	providers  ProviderLister
	hosts      HostLister
	policies   PolicyLister
	models     ModelLister
	hostKeys   HostKeyLister
	rateLimits RateLimitLister
	keys       KeyLister
	pricings   PricingLister
	bindings   BindingLister

	// overlays is optional (nil = feature dormant): set via UseOverlays
	// from the composition root so existing New callers (tests,
	// catalog-embed) stay untouched.
	overlays OverlayLister

	// teams/projects are optional (nil = tenancy dormant): set via
	// UseTenancy from the composition root so existing New callers stay
	// untouched.
	teams           TeamLister
	projects        ProjectLister
	serviceAccounts ServiceAccountLister
	groups          GroupLister
	roles           RoleLister
	roleBindings    RoleBindingLister
	policyBindings  PolicyBindingLister

	// tokenVersions is optional (nil = every token version reads as
	// absent, which rejects tokens): set via UseTokenVersions.
	tokenVersions TokenVersionLister

	// now is the clock every snapshot inherits. Nil is the wall clock;
	// UseClock installs a test one so a key's grace-window transition is
	// reachable without sleeping.
	now func() time.Time

	snap  atomic.Pointer[Snapshot]
	ready atomic.Bool
	rmu   sync.Mutex

	settings settingsHolder
}

// Per-package narrow Lister interfaces — Catalog only needs List from each.
// Declared here on the consumer side so the concrete Store types satisfy
// them implicitly.

type ProviderLister interface {
	List(ctx context.Context) ([]*provider.Provider, error)
}
type HostLister interface {
	List(ctx context.Context) ([]*host.Host, error)
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
type KeyLister interface {
	List(ctx context.Context) ([]*key.Key, error)
}
type PricingLister interface {
	List(ctx context.Context) ([]*pricing.Pricing, error)
}
type BindingLister interface {
	List(ctx context.Context) ([]*binding.Binding, error)
}
type OverlayLister interface {
	List(ctx context.Context) ([]*overlay.Overlay, error)
}
type TeamLister interface {
	List(ctx context.Context) ([]*team.Team, error)
}
type ProjectLister interface {
	List(ctx context.Context) ([]*project.Project, error)
}
type ServiceAccountLister interface {
	List(ctx context.Context) ([]*serviceaccount.ServiceAccount, error)
}
type GroupLister interface {
	List(ctx context.Context) ([]*group.Group, error)
}
type RoleLister interface {
	List(ctx context.Context) ([]*role.Role, error)
}
type RoleBindingLister interface {
	List(ctx context.Context) ([]*rolebinding.RoleBinding, error)
}
type PolicyBindingLister interface {
	List(ctx context.Context) ([]*policybinding.PolicyBinding, error)
}

// TokenVersionLister reads users.token_version for every user. Satisfied by
// *app/user.Store.
type TokenVersionLister interface {
	TokenVersions(ctx context.Context) (map[string]int, error)
}

// New constructs a Catalog backed by the supplied stores. Initial Snapshot
// is empty; call Reload before serving traffic.
func New(
	providers ProviderLister,
	hosts HostLister,
	policies PolicyLister,
	models ModelLister,
	hostKeys HostKeyLister,
	rateLimits RateLimitLister,
	keys KeyLister,
	pricings PricingLister,
	bindings BindingLister,
) *Catalog {
	c := &Catalog{
		providers:  providers,
		hosts:      hosts,
		policies:   policies,
		models:     models,
		hostKeys:   hostKeys,
		rateLimits: rateLimits,
		keys:       keys,
		pricings:   pricings,
		bindings:   bindings,
	}
	c.snap.Store(&Snapshot{})
	return c
}

// UseClock replaces the wall clock every snapshot reads. Called once at
// composition time, before the first Reload.
func (c *Catalog) UseClock(now func() time.Time) { c.now = now }

// UseOverlays attaches the overlay source. Called once at composition
// time before the first Reload; nil (the default) keeps overlays dormant.
func (c *Catalog) UseOverlays(l OverlayLister) { c.overlays = l }

// UseTenancy attaches the Team, Project, ServiceAccount, Group, Role,
// RoleBinding and PolicyBinding sources. Called once at composition time
// before the first Reload; nil (the default) keeps tenancy dormant.
func (c *Catalog) UseTenancy(t TeamLister, p ProjectLister, sa ServiceAccountLister, g GroupLister,
	r RoleLister, rb RoleBindingLister, pb PolicyBindingLister) {
	c.teams, c.projects, c.serviceAccounts, c.groups = t, p, sa, g
	c.roles, c.roleBindings, c.policyBindings = r, rb, pb
}

// UseTokenVersions attaches the per-user token-version source. Called once
// at composition time before the first Reload; nil keeps every version
// absent, which makes verification reject tokens.
func (c *Catalog) UseTokenVersions(l TokenVersionLister) { c.tokenVersions = l }

// Current returns the live Snapshot. Safe to call from any goroutine; the
// returned pointer is immutable until the next successful Reload.
func (c *Catalog) Current() *Snapshot { return c.snap.Load() }

// IsReady reports whether the catalog has built its first snapshot
// successfully. Until then, the in-memory snapshot is the zero-value
// returned by New — empty maps that look identical to a legitimately
// empty catalog. The inference plane uses this to refuse traffic with
// 503 instead of silently serving "no models found" lookups.
func (c *Catalog) IsReady() bool { return c.ready.Load() }

// markReady is called by Reload after the first successful snapshot
// swap. Subsequent reloads don't toggle it back — once ready, stays
// ready. A failed reload after that point keeps serving the previous
// snapshot per the existing contract.
func (c *Catalog) markReady() { c.ready.Store(true) }
