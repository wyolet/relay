package plan

import (
	"context"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

// Input kinds, loaded from the stores.
const (
	KindProvider       engine.Kind = "provider"
	KindHost           engine.Kind = "host"
	KindModel          engine.Kind = "model"
	KindBinding        engine.Kind = "binding"
	KindPricing        engine.Kind = "pricing"
	KindOverlay        engine.Kind = "overlay"
	KindPolicy         engine.Kind = "policy"
	KindPolicyBinding  engine.Kind = "policybinding"
	KindRateLimit      engine.Kind = "ratelimit"
	KindBudget         engine.Kind = "budget"
	KindPool           engine.Kind = "pool"
	KindAccount        engine.Kind = "account"
	KindCredential     engine.Kind = "credential"
	KindTeam           engine.Kind = "team"
	KindProject        engine.Kind = "project"
	KindServiceAccount engine.Kind = "serviceaccount"
	KindUser           engine.Kind = "user"
	KindKey            engine.Kind = "key"
)

// Derived kinds, built by the controllers.
const (
	KindCatalogIndex     engine.Kind = "catalogindex"
	KindExpandedPolicy   engine.Kind = "expandedpolicy"
	KindRungPolicy       engine.Kind = "rungpolicy"
	KindAccountPlan      engine.Kind = "accountplan"
	KindKeySlot          engine.Kind = "keyslot"
	KindPoolPlan         engine.Kind = "poolplan"
	KindOwnedCredentials engine.Kind = "ownedcredentials"
	KindCallerPlan       engine.Kind = "callerplan"
	KindKeyPlan          engine.Kind = "keyplan"
	KindKeyPlanShard     engine.Kind = "keyplanshard"
	KindKeyPlanIndex     engine.Kind = "keyplanindex"
)

// Sources are the stores each input kind loads from. A nil field loads nothing.
type Sources struct {
	Providers       engine.Source[provider.Provider]
	Hosts           engine.Source[host.Host]
	Models          engine.Source[model.Model]
	Bindings        engine.Source[binding.Binding]
	Pricings        engine.Source[pricing.Pricing]
	Overlays        engine.Source[overlay.Overlay]
	Policies        engine.Source[policy.Policy]
	PolicyBindings  engine.Source[policybinding.PolicyBinding]
	RateLimits      engine.Source[ratelimit.RateLimit]
	Budgets         engine.Source[budget.Budget]
	Pools           engine.Source[pool.Pool]
	Accounts        engine.Source[account.Account]
	Credentials     engine.Source[hostkey.HostKey]
	Teams           engine.Source[team.Team]
	Projects        engine.Source[project.Project]
	ServiceAccounts engine.Source[serviceaccount.ServiceAccount]
	Users           engine.Source[user.User]
	Keys            engine.Source[key.Key]
}

// Register declares every input kind with its source, the derived kinds, and their controllers on e. cells must be the registry passed as e's Options.Cells, so the cells the controllers acquire are kept alive by the plans that link them. Call before e.Load.
func Register(e *engine.Engine, cells *cell.Registry, src Sources) {
	input(e, KindProvider, src.Providers, metaOf(func(r *provider.Provider) meta.Metadata { return r.Meta }), 1)
	input(e, KindHost, src.Hosts, metaOf(func(r *host.Host) meta.Metadata { return r.Meta }), 1)
	input(e, KindModel, src.Models, metaOf(func(r *model.Model) meta.Metadata { return r.Meta }), 16)
	input(e, KindBinding, src.Bindings, metaOf(func(r *binding.Binding) meta.Metadata { return r.Meta }), 16)
	input(e, KindPricing, src.Pricings, metaOf(func(r *pricing.Pricing) meta.Metadata { return r.Meta }), 16)
	input(e, KindOverlay, src.Overlays, func(o *overlay.Overlay) engine.Meta { return engine.Meta{ID: OverlayID(o.Kind, o.ResourceID)} }, 1)
	input(e, KindPolicy, src.Policies, metaOf(func(r *policy.Policy) meta.Metadata { return r.Meta }), 16)
	input(e, KindPolicyBinding, src.PolicyBindings, metaOf(func(r *policybinding.PolicyBinding) meta.Metadata { return r.Meta }), 16)
	input(e, KindRateLimit, src.RateLimits, metaOf(func(r *ratelimit.RateLimit) meta.Metadata { return r.Meta }), 1)
	input(e, KindBudget, src.Budgets, metaOf(func(r *budget.Budget) meta.Metadata { return r.Meta }), 1)
	input(e, KindPool, src.Pools, metaOf(func(r *pool.Pool) meta.Metadata { return r.Meta }), 1)
	input(e, KindAccount, src.Accounts, metaOf(func(r *account.Account) meta.Metadata { return r.Meta }), 1)
	// Version 0 makes the engine fetch and compare credential rows by value: a credential's resolved secret can change while its row version does not (an env var edited, a 401 heal re-resolving it), and a versioned Apply of the same version would be dropped.
	input(e, KindCredential, src.Credentials, func(r *hostkey.HostKey) engine.Meta { return engine.Meta{ID: r.Meta.ID, Name: r.Meta.Name} }, 16)
	input(e, KindTeam, src.Teams, metaOf(func(r *team.Team) meta.Metadata { return r.Meta }), 1)
	input(e, KindProject, src.Projects, metaOf(func(r *project.Project) meta.Metadata { return r.Meta }), 16)
	input(e, KindServiceAccount, src.ServiceAccounts, metaOf(func(r *serviceaccount.ServiceAccount) meta.Metadata { return r.Meta }), 64)
	input(e, KindUser, src.Users, func(u *user.User) engine.Meta {
		return engine.Meta{ID: u.ID, Name: u.Username, Version: version(u.ResourceVersion)}
	}, 64)
	input(e, KindKey, src.Keys, metaOf(func(r *key.Key) meta.Metadata { return r.Meta }), 256)

	derived[CatalogIndex](e, KindCatalogIndex, 1)
	derived[ExpandedPolicy](e, KindExpandedPolicy, 16)
	derived[RungPolicy](e, KindRungPolicy, 64)
	derived[AccountPlan](e, KindAccountPlan, 1)
	derived[KeySlot](e, KindKeySlot, 16)
	derived[PoolPlan](e, KindPoolPlan, 1)
	derived[OwnedCredentials](e, KindOwnedCredentials, 16)
	derived[CallerPlan](e, KindCallerPlan, 64)
	derived[KeyPlan](e, KindKeyPlan, 256)
	derived[KeyPlanShard](e, KindKeyPlanShard, 16)
	derived[KeyPlanIndex](e, KindKeyPlanIndex, 1)

	c := &controllers{cells: cells}
	for _, ctl := range c.all() {
		e.Register(ctl)
	}
}

// OverlayID is an overlay's engine id: overlays are keyed by the kind and id of the row they patch.
func OverlayID(kind, resourceID string) string { return kind + "/" + resourceID }

func input[T any](e *engine.Engine, k engine.Kind, src engine.Source[T], m func(*T) engine.Meta, shards int) {
	if src == nil {
		src = noRows[T]{}
	}
	engine.RegisterKind(e, engine.KindSpec[T]{Kind: k, Meta: m, Source: src, Shards: shards})
}

func derived[T any](e *engine.Engine, k engine.Kind, shards int) {
	engine.RegisterKind(e, engine.KindSpec[T]{Kind: k, Shards: shards})
}

func metaOf[T any](md func(*T) meta.Metadata) func(*T) engine.Meta {
	return func(r *T) engine.Meta {
		m := md(r)
		return engine.Meta{ID: m.ID, Name: m.Name, Version: version(m.ResourceVersion)}
	}
}

// version reads a row's resource version; a missing or unreadable one is 0, which makes the engine fetch on every event.
func version(rv string) int64 {
	v, _, _ := meta.Metadata{ResourceVersion: rv}.ExpectedVersion()
	return v
}

// noRows is the source of a kind the caller supplied none for.
type noRows[T any] struct{}

func (noRows[T]) List(context.Context) ([]*T, error)      { return nil, nil }
func (noRows[T]) Get(context.Context, string) (*T, error) { return nil, nil }

// OverlaySource adapts the overlay store, keyed by (kind, resource id), to the engine's single-string ids from OverlayID.
type OverlaySource struct{ Store *overlay.Store }

func (s OverlaySource) List(ctx context.Context) ([]*overlay.Overlay, error) {
	return s.Store.List(ctx)
}

func (s OverlaySource) Get(ctx context.Context, id string) (*overlay.Overlay, error) {
	kind, resourceID, ok := strings.Cut(id, "/")
	if !ok {
		return nil, nil
	}
	return s.Store.Get(ctx, kind, resourceID)
}

// Every other store already lists, and fetches by id with (nil, nil) for a missing row, so it is a Source as it is.
var (
	_ engine.Source[provider.Provider]             = (*provider.Store)(nil)
	_ engine.Source[host.Host]                     = (*host.Store)(nil)
	_ engine.Source[model.Model]                   = (*model.Store)(nil)
	_ engine.Source[binding.Binding]               = (*binding.Store)(nil)
	_ engine.Source[pricing.Pricing]               = (*pricing.Store)(nil)
	_ engine.Source[overlay.Overlay]               = OverlaySource{}
	_ engine.Source[policy.Policy]                 = (*policy.Store)(nil)
	_ engine.Source[policybinding.PolicyBinding]   = (*policybinding.Store)(nil)
	_ engine.Source[ratelimit.RateLimit]           = (*ratelimit.Store)(nil)
	_ engine.Source[budget.Budget]                 = (*budget.Store)(nil)
	_ engine.Source[pool.Pool]                     = (*pool.Store)(nil)
	_ engine.Source[account.Account]               = (*account.Store)(nil)
	_ engine.Source[hostkey.HostKey]               = (*hostkey.Store)(nil)
	_ engine.Source[team.Team]                     = (*team.Store)(nil)
	_ engine.Source[project.Project]               = (*project.Store)(nil)
	_ engine.Source[serviceaccount.ServiceAccount] = (*serviceaccount.Store)(nil)
	_ engine.Source[user.User]                     = (*user.Store)(nil)
	_ engine.Source[key.Key]                       = (*key.Store)(nil)
)
