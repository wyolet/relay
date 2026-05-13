// Package policy is the domain layer for the Policy entity — the hub that
// glues a Provider to a set of usable ProviderKeys, a set of allowed Models,
// rate-limit attachments, and a key-selection strategy.
//
// Policies are user-owned (or system-bundled). A Provider points to a default
// Policy; a RelayKey may override that with its own PolicyRef.
//
// All cross-refs are stored as ids. Cross-entity rules — every ProviderKey in
// Spec.ProviderKeyIDs belongs to Spec.ProviderID, etc. — live in the
// composition layer, not here.
package policy

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

// Policy is the routing/auth bundle for a Provider.
type Policy struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is the body. ProviderID is the FK; everything else is scoped to it.
type Spec struct {
	// ProviderID is the Provider this Policy applies to. Required.
	ProviderID string `json:"providerId" yaml:"providerId" validate:"required,uuid"`

	// ProviderKeyIDs is the explicit, ordered list of ProviderKeys eligible
	// to serve this Policy. May be empty if ProviderKeySelector is non-empty.
	ProviderKeyIDs []string `json:"providerKeyIds,omitempty" yaml:"providerKeyIds,omitempty" validate:"omitempty,dive,uuid"`

	// ProviderKeySelector picks ProviderKeys by Metadata.Labels. Merged with
	// ProviderKeyIDs at request time; entries appearing in both are deduped.
	ProviderKeySelector map[string]string `json:"providerKeySelector,omitempty" yaml:"providerKeySelector,omitempty"`

	// ModelIDs is the allowed-list of Model ids callable through this Policy.
	// Empty/nil means "any Model registered for Spec.ProviderID."
	ModelIDs []string `json:"modelIds,omitempty" yaml:"modelIds,omitempty" validate:"omitempty,dive,uuid"`

	// RateLimits attach RateLimit rule sets that apply at request time.
	RateLimits []ratelimit.Attachment `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty" validate:"omitempty,dive"`

	// SkipDefaultLimits opts out of the implicit "every Policy targeting an
	// auth-required Provider must enforce at least requests + tokens" rule.
	// Used by special-purpose policies that have their limits enforced
	// elsewhere (e.g. system-owned passthrough policies).
	SkipDefaultLimits bool `json:"skipDefaultLimits,omitempty" yaml:"skipDefaultLimits,omitempty"`

	// KeySelection is the algorithm used to pick a ProviderKey from the
	// healthy pool. Defaults to prioritized.
	KeySelection KeySelection `json:"keySelection,omitempty" yaml:"keySelection,omitempty" validate:"omitempty,oneof=prioritized round-robin least-recently-used"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// KeySelection controls how the keypool picks a ProviderKey from the healthy
// candidates that satisfy a Policy.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order — the first
	// healthy key in ProviderKeyIDs wins. Default — matches the most common
	// operator request ("burn key 1 before key 2").
	KeySelectionPrioritized KeySelection = "prioritized"
	// KeySelectionRoundRobin rotates evenly across healthy keys.
	KeySelectionRoundRobin KeySelection = "round-robin"
	// KeySelectionLeastRecentlyUsed prefers the key whose last successful use
	// was furthest in the past.
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
//   - Owner.Kind is user or system (Policies are never provider-owned).
//   - ProviderKeyIDs contains no duplicates.
//   - ModelIDs contains no duplicates.
//
// Cross-entity checks — every referenced ProviderKey/Model belongs to the
// same Provider; ProviderID resolves; RateLimits resolve; auth-required
// Providers have at least one resolvable ProviderKey — live in the
// composition layer.
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
	if err := uniqueIDs("providerKeyIds", p.Meta.Name, p.Spec.ProviderKeyIDs); err != nil {
		return err
	}
	if err := uniqueIDs("modelIds", p.Meta.Name, p.Spec.ModelIDs); err != nil {
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
