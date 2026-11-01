// Package catalogview builds consumer-facing read projections of catalog
// data for the admin/UX surface ("API UX"): a model's hosts, its pricing per
// host, the policies that grant it. These back resource-navigation endpoints
// (GET /models/{ref}/hosts, .../pricing, .../policies) and, later, ?expand=
// on the base model read — the same functions serve both.
//
// Source is Postgres (the entity Stores), NOT the in-memory snapshot:
// detail/UX views intentionally reflect the full persisted state, including
// disabled rows, the way the CRUD APIs do. This also keeps the package fully
// decoupled from the hot path — it never touches app/catalog or app/routing.
// (A lint-rule guard forbids routing/pipeline/keypool from importing it.)
//
// Composition only. No HTTP, no chi/huma — the control plane binds these
// functions to routes. Pure-ish: I/O is the store reads; no mutation.
package catalogview

import (
	"context"
	"errors"
	"sort"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// ErrNotFound is returned when {ref} resolves to no model/host. Handlers map
// it to 404.
var ErrNotFound = errors.New("catalogview: not found")

// Narrow store interfaces — catalogview depends only on the reads it needs,
// not the fat *catalog.Stores. The concrete app/X.Store types satisfy these.
type (
	modelStore interface {
		List(context.Context) ([]*model.Model, error)
	}
	hostStore interface {
		List(context.Context) ([]*host.Host, error)
	}
	bindingStore interface {
		List(context.Context) ([]*binding.Binding, error)
	}
	pricingStore interface {
		List(context.Context) ([]*pricing.Pricing, error)
	}
	policyStore interface {
		List(context.Context) ([]*policy.Policy, error)
	}
	rateLimitStore interface {
		List(context.Context) ([]*ratelimit.RateLimit, error)
	}
	providerStore interface {
		List(context.Context) ([]*provider.Provider, error)
	}
	hostKeyStore interface {
		List(context.Context) ([]*hostkey.HostKey, error)
	}
	poolStore interface {
		List(context.Context) ([]*pool.Pool, error)
	}
)

// Service composes the store reads into views. Construct once with the
// deployment's stores; methods are read-only.
type Service struct {
	Models     modelStore
	Hosts      hostStore
	Bindings   bindingStore
	Pricings   pricingStore
	Policies   policyStore
	RateLimits rateLimitStore
	Providers  providerStore
	HostKeys   hostKeyStore
	// Pools is optional: without it a policy's pools reach no host.
	Pools poolStore
	// Accounts is optional: without it every account projection is a 404.
	Accounts accountStore

	// Visible optionally scopes rows per owner: policy-rooted projections
	// 404 on a policy the caller may not see, and host-key / policy rows
	// inside other projections are dropped. Nil leaves every row visible
	// (the single-user default). kind is the singular resource kind
	// ("policy", "host-key"); id is that row's metadata.id.
	Visible func(kind, id string, owner meta.Owner) bool
}

func (s *Service) visible(kind, id string, owner meta.Owner) bool {
	if s.Visible == nil {
		return true
	}
	return s.Visible(kind, id, owner)
}

// ── view shapes ─────────────────────────────────────────────────────────────

type HostRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	BaseURL     string `json:"baseURL,omitempty"`
	// Enabled + Icon are intrinsic host properties, carried on every HostRef
	// so "host off" flags and real logos render everywhere a host appears.
	Enabled bool      `json:"enabled"`
	Icon    *IconView `json:"icon,omitempty"`
}
type IconView struct {
	Path string `json:"path,omitempty"`
}
type ModelRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Capabilities/context/deprecation are intrinsic model properties, so
	// they ride along on every ModelRef — the policy, host, and model tabs
	// all render the same capability icons + context window from one shape.
	Capabilities       []string         `json:"capabilities,omitempty"`
	ContextWindowTotal int              `json:"contextWindowTotal,omitempty"`
	ContextWindowInput int              `json:"contextWindowInput,omitempty"`
	Deprecation        *DeprecationView `json:"deprecation,omitempty"`
}
type ProviderRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}
type DeprecationView struct {
	Status      string `json:"status"`
	SunsetDate  string `json:"sunsetDate,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}
type OwnerRef struct {
	Kind string `json:"kind"`         // "user" | "host" | "system" | …
	ID   string `json:"id,omitempty"` // for host-tier policies, the host id
	Name string `json:"name,omitempty"`
}
type BindingView struct {
	ID           string   `json:"id"`
	Adapter      string   `json:"adapter"`
	UpstreamName string   `json:"upstreamName,omitempty"`
	Enabled      bool     `json:"enabled"`
	Snapshots    []string `json:"snapshots,omitempty"`
}
type Rate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
	ServiceTier string  `json:"serviceTier,omitempty"`
}
type PricingView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Rates    []Rate `json:"rates"`
}
type Limit struct {
	Meter    string `json:"meter"`
	Amount   int64  `json:"amount"`
	Window   string `json:"window"`
	Strategy string `json:"strategy"`
}

// ModelHostRow — one host serving the model, with its binding + pricing.
type ModelHostRow struct {
	Host    HostRef      `json:"host"`
	Binding BindingView  `json:"binding"`
	Pricing *PricingView `json:"pricing"`
}

// ModelPriceRow — flat: one host that prices this model (host inline).
type ModelPriceRow struct {
	Host     HostRef `json:"host"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Currency string  `json:"currency"`
	Rates    []Rate  `json:"rates"`
}

// ModelPolicyRow — flat: a policy that grants this model. Host-tier policies
// name their host via Owner; user policies grant across hosts (Owner.Kind
// "user"). Limits are the rules every rate limit linked for THIS model adds.
type ModelPolicyRow struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Owner  OwnerRef `json:"owner"`
	Limits []Limit  `json:"limits"`
}

// ── projections ─────────────────────────────────────────────────────────────

// ModelHosts returns the hosts serving the model (by id or slug), each with
// its binding and resolved pricing.
func (s *Service) ModelHosts(ctx context.Context, ref string) (ModelRef, []ModelHostRow, error) {
	m, idx, err := s.load(ctx, ref)
	if err != nil {
		return ModelRef{}, nil, err
	}
	rows := []ModelHostRow{}
	for _, b := range idx.bindingsByModel[m.Meta.ID] {
		h, ok := idx.hostByID[b.Spec.HostID]
		if !ok {
			continue
		}
		rows = append(rows, ModelHostRow{
			Host:    hostRefOf(h),
			Binding: bindingViewOf(b),
			Pricing: idx.pricingFor(b),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host.Name < rows[j].Host.Name })
	return modelRefOf(m), rows, nil
}

// ModelPricing returns the model's pricing per host, flat (host inline).
func (s *Service) ModelPricing(ctx context.Context, ref string) (ModelRef, []ModelPriceRow, error) {
	m, idx, err := s.load(ctx, ref)
	if err != nil {
		return ModelRef{}, nil, err
	}
	rows := []ModelPriceRow{}
	for _, b := range idx.bindingsByModel[m.Meta.ID] {
		h, ok := idx.hostByID[b.Spec.HostID]
		if !ok {
			continue
		}
		pv := idx.pricingFor(b)
		if pv == nil {
			continue // a pricing tab lists priced hosts
		}
		rows = append(rows, ModelPriceRow{Host: hostRefOf(h), ID: pv.ID, Name: pv.Name, Currency: pv.Currency, Rates: pv.Rates})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host.Name < rows[j].Host.Name })
	return modelRefOf(m), rows, nil
}

// ModelPolicies returns the policies that grant this model, flat: on a host
// the model is bound to and the policy reaches, its model list is empty or
// matches. Limits are the rules of every linked rate limit that applies to
// the model there, resolved to numbers.
func (s *Service) ModelPolicies(ctx context.Context, ref string) (ModelRef, []ModelPolicyRow, error) {
	m, idx, err := s.load(ctx, ref)
	if err != nil {
		return ModelRef{}, nil, err
	}
	provSlug := idx.providerSlug // best-effort; "" when owner unknown
	modelHosts := idx.modelHostSet(m.Meta.ID)

	rows := []ModelPolicyRow{}
	for _, p := range idx.policies {
		if !s.visible("policy", p.Meta.ID, p.Meta.Owner) {
			continue
		}
		hostSlug, granted, _ := idx.policyGrantsModel(p, m, provSlug, modelHosts)
		if !granted {
			continue
		}
		rows = append(rows, ModelPolicyRow{
			ID:     p.Meta.ID,
			Name:   p.Meta.Name,
			Owner:  idx.ownerRefOf(p.Meta.Owner),
			Limits: idx.limitsFor(p, provSlug, m.Meta.Name, hostSlug),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return modelRefOf(m), rows, nil
}

// HostModelRow — one model served by a host, with its binding + pricing.
type HostModelRow struct {
	Model   ModelRef     `json:"model"`
	Binding BindingView  `json:"binding"`
	Pricing *PricingView `json:"pricing"`
}

// HostModels returns the models a host serves (by id or slug).
func (s *Service) HostModels(ctx context.Context, ref string) (HostRef, []HostModelRow, error) {
	h, idx, err := s.loadHost(ctx, ref)
	if err != nil {
		return HostRef{}, nil, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return HostRef{}, nil, err
	}
	modelByID := make(map[string]*model.Model, len(models))
	for _, m := range models {
		modelByID[m.Meta.ID] = m
	}
	rows := []HostModelRow{}
	for _, bs := range idx.bindingsByModel {
		for _, b := range bs {
			if b.Spec.HostID != h.Meta.ID {
				continue
			}
			m, ok := modelByID[b.Spec.ModelID]
			if !ok {
				continue
			}
			rows = append(rows, HostModelRow{Model: modelRefOf(m), Binding: bindingViewOf(b), Pricing: idx.pricingFor(b)})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model.Name < rows[j].Model.Name })
	return hostRefOf(h), rows, nil
}
