package catalogview

import (
	"context"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// index is an ad-hoc graph built per request from one read of each store.
// PG-backed (full state incl. disabled rows) — the admin/detail contract.
type index struct {
	hostByID           map[string]*host.Host
	bindingsByModel    map[string][]*binding.Binding
	pricingByID        map[string]*pricing.Pricing
	pricingByModelHost map[string]*pricing.Pricing // key modelID|hostID
	policies           []*policy.Policy
	rlByID             map[string]*ratelimit.RateLimit
	providerByID       map[string]*provider.Provider
	providerSlug       string // the resolved model's provider slug, for DSL/RL matching
}

// load resolves the model {ref} (id or slug) and builds the index. Returns
// ErrNotFound when ref matches no model.
func (s *Service) load(ctx context.Context, ref string) (*model.Model, *index, error) {
	models, err := s.Models.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	var m *model.Model
	for _, x := range models {
		if x.Meta.ID == ref || x.Meta.Name == ref {
			m = x
			break
		}
	}
	if m == nil {
		return nil, nil, ErrNotFound
	}
	idx, err := s.buildIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	if p, ok := idx.providerByID[m.Meta.Owner.ID]; ok {
		idx.providerSlug = p.Meta.Name
	}
	return m, idx, nil
}

func (s *Service) buildIndex(ctx context.Context) (*index, error) {
	hosts, err := s.Hosts.List(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := s.Bindings.List(ctx)
	if err != nil {
		return nil, err
	}
	pricings, err := s.Pricings.List(ctx)
	if err != nil {
		return nil, err
	}
	pols, err := s.Policies.List(ctx)
	if err != nil {
		return nil, err
	}
	rls, err := s.RateLimits.List(ctx)
	if err != nil {
		return nil, err
	}
	provs, err := s.Providers.List(ctx)
	if err != nil {
		return nil, err
	}

	idx := &index{
		hostByID:           make(map[string]*host.Host, len(hosts)),
		bindingsByModel:    map[string][]*binding.Binding{},
		pricingByID:        make(map[string]*pricing.Pricing, len(pricings)),
		pricingByModelHost: map[string]*pricing.Pricing{},
		policies:           pols,
		rlByID:             make(map[string]*ratelimit.RateLimit, len(rls)),
		providerByID:       make(map[string]*provider.Provider, len(provs)),
	}
	for _, h := range hosts {
		idx.hostByID[h.Meta.ID] = h
	}
	for _, b := range bindings {
		idx.bindingsByModel[b.Spec.ModelID] = append(idx.bindingsByModel[b.Spec.ModelID], b)
	}
	for _, p := range pricings {
		idx.pricingByID[p.Meta.ID] = p
		if p.Meta.Owner.Kind == meta.OwnerHost {
			for _, mid := range p.Spec.TargetModelIDs {
				idx.pricingByModelHost[mid+"|"+p.Meta.Owner.ID] = p
			}
		}
	}
	for _, rl := range rls {
		idx.rlByID[rl.Meta.ID] = rl
	}
	for _, p := range provs {
		idx.providerByID[p.Meta.ID] = p
	}
	return idx, nil
}

// pricingFor resolves a binding's pricing: explicit PricingID first, then the
// host-owned pricing covering the (model, host) pair. nil when unpriced.
func (idx *index) pricingFor(b *binding.Binding) *PricingView {
	var p *pricing.Pricing
	if b.Spec.PricingID != "" {
		p = idx.pricingByID[b.Spec.PricingID]
	}
	if p == nil {
		p = idx.pricingByModelHost[b.Spec.ModelID+"|"+b.Spec.HostID]
	}
	if p == nil {
		return nil
	}
	out := &PricingView{ID: p.Meta.ID, Name: p.Meta.Name, Currency: p.Spec.Currency, Rates: make([]Rate, 0, len(p.Spec.Rates))}
	for _, r := range p.Spec.Rates {
		out.Rates = append(out.Rates, Rate{Meter: string(r.Meter), Unit: string(r.Unit), Amount: r.Amount, AboveTokens: r.AboveTokens})
	}
	return out
}

// modelHostSlugs returns the slugs of the hosts the model is bound to.
func (idx *index) modelHostSlugs(modelID string) []string {
	bs := idx.bindingsByModel[modelID]
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if h, ok := idx.hostByID[b.Spec.HostID]; ok {
			out = append(out, h.Meta.Name)
		}
	}
	return out
}

func (idx *index) ownerRefOf(o meta.Owner) OwnerRef {
	ref := OwnerRef{Kind: string(o.Kind), ID: o.ID}
	if o.Kind == meta.OwnerHost {
		if h, ok := idx.hostByID[o.ID]; ok {
			ref.Name = h.Meta.Name
		}
	}
	return ref
}

func (idx *index) limitsOf(rlID string) []Limit {
	if rlID == "" {
		return []Limit{}
	}
	rl, ok := idx.rlByID[rlID]
	if !ok || (rl.Spec.Enabled != nil && !*rl.Spec.Enabled) {
		return []Limit{}
	}
	out := make([]Limit, 0, len(rl.Spec.Rules))
	for _, r := range rl.Spec.Rules {
		out = append(out, Limit{Meter: string(r.Meter), Amount: r.Amount, Window: r.Window.String(), Strategy: string(r.Strategy)})
	}
	return out
}

func hostRefOf(h *host.Host) HostRef {
	return HostRef{ID: h.Meta.ID, Name: h.Meta.Name, DisplayName: h.Meta.DisplayName, BaseURL: h.Spec.BaseURL}
}
func modelRefOf(m *model.Model) ModelRef {
	return ModelRef{ID: m.Meta.ID, Name: m.Meta.Name, DisplayName: m.Meta.DisplayName}
}
func bindingViewOf(b *binding.Binding) BindingView {
	return BindingView{ID: b.Meta.ID, Adapter: string(b.Spec.Adapter), UpstreamName: b.Spec.UpstreamName, Enabled: b.IsEnabled(), Snapshots: b.Spec.Snapshots}
}
