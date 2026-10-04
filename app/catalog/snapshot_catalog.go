package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// ── Read accessors ─────────────────────────────────────────────────────────

// Provider returns the enabled Provider with this id, or false.
func (s *Snapshot) Provider(id string) (*provider.Provider, bool) {
	p, ok := s.providersByID[id]
	return p, ok
}

// ProviderByName returns the enabled Provider with this slug, or false.
func (s *Snapshot) ProviderByName(name string) (*provider.Provider, bool) {
	p, ok := s.providersByName[name]
	return p, ok
}

// ProviderSlug returns the Provider.Meta.Name for the given Provider id, or
// false. Hot-path resolution uses this to compare a Model's owning Provider
// against the suffix/header hint without needing the full Provider row.
func (s *Snapshot) ProviderSlug(providerID string) (string, bool) {
	p, ok := s.providersByID[providerID]
	if !ok {
		return "", false
	}
	return p.Meta.Name, true
}

// Host returns the enabled Host with this id, or false.
func (s *Snapshot) Host(id string) (*host.Host, bool) {
	h, ok := s.hostsByID[id]
	return h, ok
}

// HostByName returns the enabled Host with this slug, or false.
func (s *Snapshot) HostByName(name string) (*host.Host, bool) {
	h, ok := s.hostsByName[name]
	return h, ok
}

// HostSlug returns the Host.Meta.Name for the given Host id, or false.
func (s *Snapshot) HostSlug(hostID string) (string, bool) {
	h, ok := s.hostsByID[hostID]
	if !ok {
		return "", false
	}
	return h.Meta.Name, true
}

// Policy returns the enabled Policy with this id, or false.
func (s *Snapshot) Policy(id string) (*policy.Policy, bool) {
	p, ok := s.policiesByID[id]
	return p, ok
}

// DisabledPolicy returns the Policy with this id when it is present but
// switched off. Resolution calls it after Policy misses, so a credential
// pointing at a disabled policy answers 403 policy_disabled rather than
// silently resolving to something broader.
func (s *Snapshot) DisabledPolicy(id string) (*policy.Policy, bool) {
	p, ok := s.disabledPoliciesByID[id]
	return p, ok
}

// PolicyByName returns the enabled Policy with this slug, or false.
func (s *Snapshot) PolicyByName(name string) (*policy.Policy, bool) {
	p, ok := s.policiesByName[name]
	return p, ok
}

// Model returns the enabled Model with this id, or false.
func (s *Snapshot) Model(id string) (*model.Model, bool) {
	m, ok := s.modelsByID[id]
	return m, ok
}

// ModelsByName returns every enabled Model whose Meta.Name matches.
// The slug is unique per kind, but the index is multivalued to absorb
// transient overlap during reload. Customer-facing addressing uses
// SnapshotByName instead — this accessor is admin-only.
func (s *Snapshot) ModelsByName(name string) []*model.Model {
	return s.modelsByName[name]
}

// SnapshotByName returns the Model + Snapshot for a pinned snapshot name,
// or false. Snapshot names are catalog-wide unique (validation enforces no
// collision with the owning Model's name or aliases).
func (s *Snapshot) SnapshotByName(name string) (*model.Model, *model.Snapshot, bool) {
	r, ok := s.snapshotsByName[name]
	if !ok {
		return nil, nil, false
	}
	return r.Model, r.Snapshot, true
}

// AllModels returns every enabled Model in stable slug order. Used by
// the /catalog/resolve endpoint for host-only refs ("@bedrock") that
// need to walk the entire catalog rather than a single provider.
func (s *Snapshot) AllModels() []*model.Model {
	out := make([]*model.Model, 0, len(s.modelsByID))
	for _, m := range s.modelsByID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// ModelsByProvider returns every enabled Model whose owning Provider
// matches providerID. Stable order by slug. Used by the /catalog/resolve
// admin endpoint to enumerate a provider's catalog.
func (s *Snapshot) ModelsByProvider(providerID string) []*model.Model {
	out := make([]*model.Model, 0)
	for _, m := range s.modelsByID {
		if m.Meta.Owner.ID == providerID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllProviders / AllPolicies / AllHostKeys / AllKeys / AllRateLimits
// / AllPricings return the full enabled set in stable slug order. Used
// by the debug-snapshot endpoint; never on the hot path.

// AllProviders returns every Provider in the snapshot, sorted by slug.
func (s *Snapshot) AllProviders() []*provider.Provider {
	out := make([]*provider.Provider, 0, len(s.providersByID))
	for _, p := range s.providersByID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllPolicies returns every Policy in the snapshot, sorted by slug.
func (s *Snapshot) AllPolicies() []*policy.Policy {
	out := make([]*policy.Policy, 0, len(s.policiesByID))
	for _, p := range s.policiesByID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllRateLimits returns every RateLimit in the snapshot, sorted by slug.
func (s *Snapshot) AllRateLimits() []*ratelimit.RateLimit {
	out := make([]*ratelimit.RateLimit, 0, len(s.rateLimitsByID))
	for _, r := range s.rateLimitsByID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllPricings returns every Pricing in the snapshot, sorted by slug.
func (s *Snapshot) AllPricings() []*pricing.Pricing {
	out := make([]*pricing.Pricing, 0, len(s.pricingsByID))
	for _, p := range s.pricingsByID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// RateLimit returns the enabled RateLimit with this id, or false.
func (s *Snapshot) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	r, ok := s.rateLimitsByID[id]
	return r, ok
}

// RateLimitByName returns the enabled RateLimit with this slug, or
// false. Used by proxy-mode dispatch to look up the system-owned
// inference-api-proxy / inference-api-proxy-anonymous buckets.
func (s *Snapshot) RateLimitByName(name string) (*ratelimit.RateLimit, bool) {
	r, ok := s.rateLimitsByName[name]
	return r, ok
}

// Hosts returns all enabled Host rows. Stable order by slug. Used by
// the /v1/proxy/hosts list endpoint.
func (s *Snapshot) Hosts() []*host.Host {
	out := make([]*host.Host, 0, len(s.hostsByID))
	for _, h := range s.hostsByName {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// ModelsInPolicy returns the Models attached to this Policy in declaration
// order. nil if the Policy is unknown or empty.
func (s *Snapshot) ModelsInPolicy(policyID string) []*model.Model {
	return s.modelsByPolicy[policyID]
}

// HostKeysInPolicy returns the HostKeys attached to this Policy in
// declaration order (relevant for KeySelectionPrioritized).
func (s *Snapshot) HostKeysInPolicy(policyID string) []*hostkey.HostKey {
	return s.hostKeysByPolicy[policyID]
}

// RateLimitOfPolicy returns the single RateLimit bound to this Policy, or
// nil when none is configured.
func (s *Snapshot) RateLimitOfPolicy(policyID string) *ratelimit.RateLimit {
	return s.rateLimitByPolicy[policyID]
}

// Pricing returns the enabled Pricing with this id, or false.
func (s *Snapshot) Pricing(id string) (*pricing.Pricing, bool) {
	p, ok := s.pricingsByID[id]
	return p, ok
}

// PriceByModelHost returns the Pricing that covers (modelID, hostID), or false.
func (s *Snapshot) PriceByModelHost(modelID, hostID string) (*pricing.Pricing, bool) {
	p, ok := s.pricingByModelHost[modelID+"|"+hostID]
	return p, ok
}

// PricingForBinding resolves the rate sheet billing against a binding: the
// binding's explicit Spec.PricingID first, else the host-owned pricing
// covering the (model, host) pair. Mirrors catalogview's resolution so the
// admin read-projection and the emit-time cost stamp agree.
func (s *Snapshot) PricingForBinding(b *binding.Binding) (*pricing.Pricing, bool) {
	if b == nil {
		return nil, false
	}
	if b.Spec.PricingID != "" {
		if p, ok := s.pricingsByID[b.Spec.PricingID]; ok {
			return p, true
		}
	}
	p, ok := s.pricingByModelHost[b.Spec.ModelID+"|"+b.Spec.HostID]
	return p, ok
}

// Binding returns the enabled HostBinding with this id, or false.
func (s *Snapshot) Binding(id string) (*binding.Binding, bool) {
	b, ok := s.bindingsByID[id]
	return b, ok
}

// BindingForModelHost returns the binding for (modelID, hostID), or false.
// O(1) — the routing hot-path lookup.
func (s *Snapshot) BindingForModelHost(modelID, hostID string) (*binding.Binding, bool) {
	b, ok := s.bindingsByModelHost[modelID+"|"+hostID]
	return b, ok
}

// BindingsForModel returns the bindings declared for a model, sorted by
// binding name. The returned slice must not be mutated.
func (s *Snapshot) BindingsForModel(modelID string) []*binding.Binding {
	return s.bindingsByModel[modelID]
}

// AllBindings returns every binding in the snapshot, sorted by name.
func (s *Snapshot) AllBindings() []*binding.Binding {
	out := make([]*binding.Binding, 0, len(s.bindingsByID))
	for _, b := range s.bindingsByID {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}
