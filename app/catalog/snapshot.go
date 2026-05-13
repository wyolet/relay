// Package catalog is the composition layer: it pulls the six entity stores
// together into an atomic in-memory Snapshot the request path can read in
// O(1). Snapshots are immutable; Reload rebuilds and atomically swaps.
//
// Membership rules (locked in design, not configuration):
//
//   - Provider: never in the snapshot. Base URLs and wire-shape selection
//     live in code, keyed off the provider slug; PG rows are informational
//     for the admin UI.
//   - Policy: enabled rows only.
//   - Model: enabled rows referenced by ≥1 enabled Policy.
//   - ProviderKey: enabled rows referenced by ≥1 enabled Policy.
//   - RateLimit: enabled rows referenced by ≥1 enabled Policy.
//   - RelayKey: enabled rows. (Auth proceeds even if its Policy is
//     disabled, so the response can say "policy disabled" instead of 401.)
//
// Disabled rows are evicted at the next Reload; flipping Spec.Enabled is
// the entire toggle mechanism.
package catalog

import (
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/providerkey"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// Snapshot is the immutable in-memory view. All maps are populated at
// construction by Reload and never written after — read accessors are
// safe to call from any goroutine.
type Snapshot struct {
	policiesByID   map[string]*policy.Policy
	policiesByName map[string]*policy.Policy

	modelsByID   map[string]*model.Model
	modelsByName map[string]*model.Model // includes Spec.Aliases

	providerKeysByID map[string]*providerkey.ProviderKey

	rateLimitsByID map[string]*ratelimit.RateLimit

	relayKeysByID   map[string]*relaykey.RelayKey
	relayKeysByHash map[string]*relaykey.RelayKey

	// Reverse joins precomputed from Policy.Spec.* lists, so the hot path
	// doesn't iterate.
	modelsByPolicy       map[string][]*model.Model
	providerKeysByPolicy map[string][]*providerkey.ProviderKey
	rateLimitByPolicy    map[string]*ratelimit.RateLimit
}

// ── Read accessors ─────────────────────────────────────────────────────────

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

// Model returns the reachable Model with this id, or false.
func (s *Snapshot) Model(id string) (*model.Model, bool) {
	m, ok := s.modelsByID[id]
	return m, ok
}

// ModelByName returns the reachable Model matching this slug or alias.
func (s *Snapshot) ModelByName(name string) (*model.Model, bool) {
	m, ok := s.modelsByName[name]
	return m, ok
}

// ProviderKey returns the reachable ProviderKey with this id, or false.
func (s *Snapshot) ProviderKey(id string) (*providerkey.ProviderKey, bool) {
	k, ok := s.providerKeysByID[id]
	return k, ok
}

// RateLimit returns the reachable RateLimit with this id, or false.
func (s *Snapshot) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	r, ok := s.rateLimitsByID[id]
	return r, ok
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

// ModelsInPolicy returns the reachable Models attached to this Policy in
// declaration order. nil if the Policy is unknown or empty.
func (s *Snapshot) ModelsInPolicy(policyID string) []*model.Model {
	return s.modelsByPolicy[policyID]
}

// ProviderKeysInPolicy returns the reachable ProviderKeys attached to this
// Policy in declaration order (relevant for KeySelectionPrioritized).
func (s *Snapshot) ProviderKeysInPolicy(policyID string) []*providerkey.ProviderKey {
	return s.providerKeysByPolicy[policyID]
}

// RateLimitOfPolicy returns the single RateLimit bound to this Policy, or
// nil when none is configured.
func (s *Snapshot) RateLimitOfPolicy(policyID string) *ratelimit.RateLimit {
	return s.rateLimitByPolicy[policyID]
}
