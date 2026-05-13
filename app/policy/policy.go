// Package policy is the domain layer for the Policy entity — the named
// group that bundles Models (m:n), ProviderKeys (m:n), and an optional
// single RateLimit (m:1, the one rule set that applies to traffic through
// this Policy).
//
// Wire format: Policy YAML/JSON carries entity *names*; the admin and seed
// boundaries resolve names to ids.
//
// Go domain shape: Policy.Spec carries id arrays / scalar id. Callers read
// the lists directly without dealing with joins.
//
// PG storage: arrays land in junction tables (policy_models,
// policy_provider_keys) with proper FKs; the single RateLimit ref is a
// column on the policies table. Policy's store.go fans the writes out
// across the junctions inside a transaction and rebuilds the arrays on
// read. (Migration + sqlc queries land as a follow-up; current store.go
// keeps Spec in JSONB until then, see TODO in store.go.)
//
// Reverse direction: RelayKey carries a single Spec.PolicyID (m:1).
// Provider names its default via Spec.DefaultPolicyID. Models and
// ProviderKeys carry no policy reference — they are discovered via the
// junctions / via Policy.Spec.
package policy

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Policy is the routing/auth bundle. Composition layer enforces that every
// id in the lists below resolves and (for ProviderKeys) shares the Provider
// of the Models in the same Policy.
type Policy struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the membership lists, the single rate-limit, and the
// strategy knobs.
type Spec struct {
	// ModelIDs is the set of Models this Policy exposes (m:n with Model).
	ModelIDs []string `json:"modelIds,omitempty" yaml:"modelIds,omitempty" validate:"omitempty,dive,uuid"`

	// ProviderKeyIDs is the set of ProviderKeys this Policy can draw from
	// (m:n with ProviderKey). Order is significant for KeySelectionPrioritized.
	ProviderKeyIDs []string `json:"providerKeyIds,omitempty" yaml:"providerKeyIds,omitempty" validate:"omitempty,dive,uuid"`

	// RateLimitID is the single RateLimit applied to traffic through this
	// Policy. Optional — empty means no policy-level rate limiting.
	RateLimitID string `json:"rateLimitId,omitempty" yaml:"rateLimitId,omitempty" validate:"omitempty,uuid"`

	// KeySelection is the algorithm used to pick a ProviderKey from the
	// healthy pool. Defaults to prioritized.
	KeySelection KeySelection `json:"keySelection,omitempty" yaml:"keySelection,omitempty" validate:"omitempty,oneof=prioritized round-robin least-recently-used"`

	// SkipDefaultLimits opts out of the implicit "every Policy targeting an
	// auth-required Provider must enforce at least requests + tokens" rule
	// performed by the composition layer.
	SkipDefaultLimits bool `json:"skipDefaultLimits,omitempty" yaml:"skipDefaultLimits,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// KeySelection controls how the keypool picks a ProviderKey from the
// healthy candidates that satisfy this Policy.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order — the first
	// healthy key in ProviderKeyIDs wins. Default.
	KeySelectionPrioritized KeySelection = "prioritized"
	// KeySelectionRoundRobin rotates evenly across healthy keys.
	KeySelectionRoundRobin KeySelection = "round-robin"
	// KeySelectionLeastRecentlyUsed prefers the key whose last successful
	// use was furthest in the past.
	KeySelectionLeastRecentlyUsed KeySelection = "least-recently-used"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Policy) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// EffectiveKeySelection returns KeySelection or the prioritized default.
func (p *Policy) EffectiveKeySelection() KeySelection {
	if p.Spec.KeySelection == "" {
		return KeySelectionPrioritized
	}
	return p.Spec.KeySelection
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is user or system.
//   - ModelIDs / ProviderKeyIDs have no within-list duplicates.
//
// Cross-entity checks (every id resolves; ProviderKeys + Models share a
// Provider; RateLimitID resolves; auth-required Providers have at least
// one resolvable ProviderKey) live in the composition layer.
func (p *Policy) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	switch p.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem:
	case meta.OwnerProvider:
		return fmt.Errorf("policy %q: owner.kind must be user or system, got provider", p.Meta.Name)
	default:
		return fmt.Errorf("policy %q: owner.kind required (user|system)", p.Meta.Name)
	}
	if err := uniqueIDs("modelIds", p.Meta.Name, p.Spec.ModelIDs); err != nil {
		return err
	}
	if err := uniqueIDs("providerKeyIds", p.Meta.Name, p.Spec.ProviderKeyIDs); err != nil {
		return err
	}
	return nil
}

func uniqueIDs(field, owner string, ids []string) error {
	if len(ids) < 2 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("policy %q: duplicate %s entry %q", owner, field, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
