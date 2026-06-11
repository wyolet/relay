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

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/slug"
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

	// snapshotsByName indexes every Model.Spec.Snapshot's Name → owning
	// Model. Used by request-time resolution to honor pinned snapshot
	// names (e.g. "gpt-4o-2024-11-20") before falling back to the bare
	// Model name and following its Pointer.
	snapshotsByName map[string]snapshotRef

	// snapshotAliases indexes every addressable form of a snapshot, all
	// slug-normalized so request input and stored key collapse identically
	// (e.g. "openai/gpt-5.4-mini" and "openai/gpt-5-4-mini" → one key). Per
	// snapshot we materialize: provider-qualified ("openai/gpt-5-4-mini"),
	// host-pinned ("gpt-5-4-mini@openai"), and both ("openai/gpt-5-4-mini@azure").
	// Host-pinned entries carry the bound HostID so resolution pins that host.
	// Checked AFTER snapshotsByName, so a real snapshot name always wins over
	// a synthesized alias.
	snapshotAliases map[string]snapshotRef

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

	// allowedCombosByPolicy[policyID] is the set of (model, host) pairs an
	// explicit-grant policy allows — built from its ModelIDs + Models refs so
	// authorization is an O(1) membership test. Implicit-wildcard policies are
	// absent (PolicyAllowsCombo returns true). See policy_allow.go.
	allowedCombosByPolicy map[string]map[comboKey]struct{}

	pricingsByID map[string]*pricing.Pricing
	// pricingByModelHost keys on modelID+"|"+hostID for O(1) hot-path lookup.
	pricingByModelHost map[string]*pricing.Pricing

	bindingsByID map[string]*binding.Binding
	// bindingsByModelHost keys on modelID+"|"+hostID for O(1) routing lookup.
	bindingsByModelHost map[string]*binding.Binding
	// bindingsByModel groups a model's bindings (sorted by name) for
	// snapshot-alias generation and per-model enumeration.
	bindingsByModel map[string][]*binding.Binding

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

// snapshotRef links a Snapshot back to its owning Model. Stored in the
// snapshot-name index so request-time lookup can return both in one shot.
// HostID is set only for host-pinned aliases (the "@host" forms) — it tells
// resolution to bind that specific host; "" means no pin (caller didn't pin a
// host, so binding selection runs as normal).
type snapshotRef struct {
	Model    *model.Model
	Snapshot *model.Snapshot
	HostID   string
}

// hostPinSkip names hosts whose models carry non-normalizable upstream names
// (Bedrock ARNs, Vertex publisher paths with colons/slashes) that slug.From
// would mangle. We skip generating host-pinned aliases for them until per-host
// upstream-name handling lands; bare + provider-qualified addressing still
// works for their models.
// TODO(bedrock/vertex): drop this skip once host-specific upstream names are
// handled in alias generation.
var hostPinSkip = map[string]struct{}{
	"amazon-bedrock": {},
	"google-vertex":  {},
}

// indexModelSnapshots materializes every addressable alias for a model's
// snapshots into the bare-name + alias indices. Providers and hosts must
// already be indexed (build order guarantees this; reconcile clones a full
// snapshot).
func (s *Snapshot) indexModelSnapshots(m *model.Model) {
	provSlug, _ := s.ProviderSlug(m.Meta.Owner.ID)
	for i := range m.Spec.Snapshots {
		snap := &m.Spec.Snapshots[i]
		base := snapshotRef{Model: m, Snapshot: snap}
		s.snapshotsByName[snap.Name] = base
		if provSlug != "" {
			s.snapshotAliases[slug.From(provSlug+"/"+snap.Name)] = base
		}
		for _, hb := range s.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			h, ok := s.hostsByID[hb.Spec.HostID]
			if !ok {
				continue
			}
			if _, skip := hostPinSkip[h.Meta.Name]; skip {
				continue
			}
			pinned := snapshotRef{Model: m, Snapshot: snap, HostID: hb.Spec.HostID}
			s.snapshotAliases[slug.From(snap.Name+"@"+h.Meta.Name)] = pinned
			if provSlug != "" {
				s.snapshotAliases[slug.From(provSlug+"/"+snap.Name+"@"+h.Meta.Name)] = pinned
			}
		}
	}
}

// deindexModelSnapshots removes a model's snapshots from both indices. Bare
// names are deleted by key; aliases are swept by owning-model id so deletion
// is robust even if the model's hosts were already evicted (the alias keys
// can no longer be recomputed in that case).
func (s *Snapshot) deindexModelSnapshots(m *model.Model) {
	for _, snap := range m.Spec.Snapshots {
		delete(s.snapshotsByName, snap.Name)
	}
	for k, ref := range s.snapshotAliases {
		if ref.Model.Meta.ID == m.Meta.ID {
			delete(s.snapshotAliases, k)
		}
	}
}

// ResolveSnapshot maps a slug-normalized model ref to its model + snapshot,
// plus an optional pinned HostID (set when the ref named a host via "@host").
// Bare snapshot names win over synthesized aliases. The caller normalizes the
// key with slug.From.
func (s *Snapshot) ResolveSnapshot(key string) (*model.Model, *model.Snapshot, string, bool) {
	if r, ok := s.snapshotsByName[key]; ok {
		return r.Model, r.Snapshot, r.HostID, true
	}
	if r, ok := s.snapshotAliases[key]; ok {
		return r.Model, r.Snapshot, r.HostID, true
	}
	return nil, nil, "", false
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

// HostKey returns the enabled HostKey with this id, or false.
func (s *Snapshot) HostKey(id string) (*hostkey.HostKey, bool) {
	k, ok := s.hostKeysByID[id]
	return k, ok
}

// AllProviders / AllPolicies / AllHostKeys / AllRelayKeys / AllRateLimits
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

// AllHostKeys returns every HostKey in the snapshot, sorted by slug.
func (s *Snapshot) AllHostKeys() []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, 0, len(s.hostKeysByID))
	for _, k := range s.hostKeysByID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllRelayKeys returns every RelayKey in the snapshot, sorted by slug.
func (s *Snapshot) AllRelayKeys() []*relaykey.RelayKey {
	out := make([]*relaykey.RelayKey, 0, len(s.relayKeysByID))
	for _, k := range s.relayKeysByID {
		out = append(out, k)
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

// HostKeysForHost returns every enabled HostKey whose Spec.HostID
// matches hostID. Order is by hostkey slug — stable across snapshots.
// Used by routing's policy-less flow (settings.Inference.AllowMissingPolicy)
// where the policy doesn't narrow the pool.
func (s *Snapshot) HostKeysForHost(hostID string) []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, 0)
	for _, k := range s.hostKeysByID {
		if k.Spec.HostID != hostID {
			continue
		}
		if !k.IsEnabled() {
			continue
		}
		out = append(out, k)
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
