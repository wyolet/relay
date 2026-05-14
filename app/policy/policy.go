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
	"github.com/wyolet/relay/app/modelref"
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
	// Models is the catalog grant — a list of ref strings parsed by
	// app/modelref. See its package doc for the grammar. Each entry can
	// match a single binding ("anthropic/claude-opus-4-7@bedrock"), all
	// hosts for a model ("anthropic/claude-opus-4-7" or trailing @*),
	// or every model under a provider ("anthropic" or "anthropic/*").
	//
	// Patterns expand against the live catalog at snapshot build time,
	// so a wildcard automatically includes models added later.
	Models []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1"`

	// ModelIDs is the legacy literal-ID grant — exact Model UUIDs, no
	// wildcards. Coexists with Models during the transition; the
	// snapshot expands both into the same bindingsByPolicy index.
	// New grants should prefer Models.
	ModelIDs []string `json:"modelIds,omitempty" yaml:"modelIds,omitempty" validate:"omitempty,dive,uuid"`

	// HostKeyIDs is the set of HostKeys this Policy can draw from
	// (m:n with ProviderKey). Order is significant for KeySelectionPrioritized.
	HostKeyIDs []string `json:"hostKeyIds,omitempty" yaml:"hostKeyIds,omitempty" validate:"omitempty,dive,uuid"`

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

	// IncludeDeprecated controls whether wildcard Models entries expand
	// to models whose Spec.Deprecation.Status is "deprecated" or
	// "sunset". Default false (deprecated models excluded). Literal
	// grants in Models always resolve regardless — if you explicitly
	// name a deprecated model by slug, you mean to grant it.
	IncludeDeprecated bool `json:"includeDeprecated,omitempty" yaml:"includeDeprecated,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// KeySelection controls how the keypool picks a ProviderKey from the
// healthy candidates that satisfy this Policy.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order — the first
	// healthy key in HostKeyIDs wins. Default.
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
//   - ModelIDs / HostKeyIDs have no within-list duplicates.
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
	case meta.OwnerHost:
		// Host-owned policies are tier definitions published by a Host as
		// part of its menu. They carry rules and (optionally) model lists
		// but never inbound grants — HostKeyIDs is meaningless for an
		// upstream tier.
		if p.Meta.Owner.ID == "" {
			return fmt.Errorf("policy %q: owner.id is required for host-owned policy", p.Meta.Name)
		}
		if len(p.Spec.HostKeyIDs) > 0 {
			return fmt.Errorf("policy %q: host-owned policies must not list hostKeyIds", p.Meta.Name)
		}
	default:
		return fmt.Errorf("policy %q: owner.kind required (user|system|host)", p.Meta.Name)
	}
	if err := uniqueIDs("modelIds", p.Meta.Name, p.Spec.ModelIDs); err != nil {
		return err
	}
	if err := uniqueIDs("hostKeyIds", p.Meta.Name, p.Spec.HostKeyIDs); err != nil {
		return err
	}
	if err := validateModelRefs(p.Meta.Name, p.Spec.Models); err != nil {
		return err
	}
	return nil
}

// validateModelRefs runs the modelref parser over every entry and
// rejects duplicates. Parser errors carry their own message; the policy
// name is prepended for context.
func validateModelRefs(policyName string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(refs))
	for _, raw := range refs {
		if _, dup := seen[raw]; dup {
			return fmt.Errorf("policy %q: duplicate models entry %q", policyName, raw)
		}
		seen[raw] = struct{}{}
		if _, err := modelref.Parse(raw); err != nil {
			return fmt.Errorf("policy %q: %w", policyName, err)
		}
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
