// Package catalog is the composition layer: it pulls the entity stores
// together into an atomic in-memory Snapshot the request path can read in
// O(1). Snapshots are immutable; Reload rebuilds and atomically swaps.
//
// Membership rule (single, simple): every enabled row of every kind enters
// the snapshot. There is no reachability filter — a Model not bound to any
// Policy still appears (it's just unreachable through PoolModels until
// someone wires it up). The "enabled" flag is the entire toggle mechanism.
//
// Reverse joins (modelsByPolicy, pricingByModelHost, etc.) are derived
// indices over the snapshot — they hold pointers to the same rows.
package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// Snapshot is the immutable in-memory view. All maps are populated at
// construction and never written after — read accessors are safe to call
// from any goroutine.
type Snapshot struct {
	providersByID   map[string]*provider.Provider
	providersByName map[string]*provider.Provider

	hostsByID   map[string]*host.Host
	hostsByName map[string]*host.Host

	policiesByID   map[string]*policy.Policy
	policiesByName map[string]*policy.Policy

	modelsByID map[string]*model.Model
	// modelsByName is multivalued: an alias may legitimately point at more
	// than one Model (e.g. "gpt-5" hosted by both openai and azure). The
	// consumer disambiguates using a suffix in the request string
	// (model@providerSlug) or the X-Relay-Provider header, falling back to
	// the caller's Policy.
	modelsByName map[string][]*model.Model

	hostKeysByID map[string]*hostkey.HostKey

	rateLimitsByID   map[string]*ratelimit.RateLimit
	rateLimitsByName map[string]*ratelimit.RateLimit

	relayKeysByID   map[string]*relaykey.RelayKey
	relayKeysByHash map[string]*relaykey.RelayKey

	// Reverse joins precomputed from Policy.Spec.* lists, so the hot path
	// doesn't iterate.
	modelsByPolicy    map[string][]*model.Model
	hostKeysByPolicy  map[string][]*hostkey.HostKey
	rateLimitByPolicy map[string]*ratelimit.RateLimit

	pricingsByID map[string]*pricing.Pricing
	// pricingByModelHost keys on modelID+"|"+hostID for O(1) hot-path lookup.
	pricingByModelHost map[string]*pricing.Pricing

	// Reverse-dependency indices: refsByX[X-id] = set of child refKeys that
	// reference this row. Used by the COW reconciler to enumerate dependents
	// when a parent is evicted. Allocated even for empty snapshots so
	// registerRefs has somewhere to write.
	refsByProvider  map[string]refSet
	refsByHost      map[string]refSet
	refsByModel     map[string]refSet
	refsByHostKey   map[string]refSet
	refsByRateLimit map[string]refSet
	refsByPolicy    map[string]refSet
}

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

// ModelsByName returns every enabled Model whose Spec.Aliases contains
// this name. The list is empty for unknown names. Multiple Models may share
// an alias when the same wire name is intentionally hosted by more than one
// Provider; consumers disambiguate with a host suffix or the
// X-Relay-Provider header.
func (s *Snapshot) ModelsByName(name string) []*model.Model {
	return s.modelsByName[name]
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

// HostKey returns the enabled HostKey with this id, or false.
func (s *Snapshot) HostKey(id string) (*hostkey.HostKey, bool) {
	k, ok := s.hostKeysByID[id]
	return k, ok
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

// RelayKey returns the enabled RelayKey with this id, or false.
func (s *Snapshot) RelayKey(id string) (*relaykey.RelayKey, bool) {
	k, ok := s.relayKeysByID[id]
	return k, ok
}

// RelayKeyByHash is the hot-path inbound-auth lookup. Returns the RelayKey
// whose Spec.KeyHash matches. Caller checks IsActive.
func (s *Snapshot) RelayKeyByHash(hash string) (*relaykey.RelayKey, bool) {
	k, ok := s.relayKeysByHash[hash]
	return k, ok
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
