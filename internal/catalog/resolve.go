package catalog

import (
	"fmt"

	"github.com/wyolet/relay/pkg/ids"
)

// resolveRefs rewrites cross-reference fields in the snapshot from slug (name)
// form to id form. Idempotent — values that already parse as a UUID are left
// alone. Returns a set of kinds whose rows were mutated so callers can persist
// the rewrite back to the underlying store on first migration.
//
// Currently handles:
//   - SecretSpec.Provider → Provider id
//   - PolicySpec.Provider → Provider id
//   - ModelSpec.Provider → Provider id
//   - ProviderSpec.DefaultPolicy → Policy id
//   - RelayKeySpec.PolicyRef → Policy id
//   - PolicySpec.Models[] → Model id
//   - RouteSpec.Models[] → Model id
//   - Deprecation.Replacement → Model id
//   - PolicySpec.Secrets[] → Secret id
//   - RateLimitAttachment.Ref on Policy/Secret/Model → RateLimit id
//
// Hard-errors on a string that is neither a known slug nor a valid UUID — that
// indicates the catalog is corrupt and silently dropping the ref would mask
// real breakage at the request path.
func resolveRefs(snap *snapshot) (mutated mutatedKinds, err error) {
	// Build a slug→id lookup for Providers. Snapshot's primary map is slug-keyed,
	// so this is just a field read per provider.
	provIDBySlug := make(map[string]string, len(snap.providers))
	for slug, p := range snap.providers {
		if p.Metadata.ID == "" {
			// A provider row missing an id is a separate bug; skip rather than
			// rewrite refs to an empty string.
			continue
		}
		provIDBySlug[slug] = p.Metadata.ID
	}

	resolveProv := func(field, ownerKind, ownerName string, val *string) error {
		if *val == "" {
			return nil
		}
		if ids.Valid(*val) {
			return nil
		}
		id, ok := provIDBySlug[*val]
		if !ok {
			return fmt.Errorf("%s %q: %s references unknown provider %q",
				ownerKind, ownerName, field, *val)
		}
		*val = id
		return nil
	}

	for name, sec := range snap.secrets {
		before := sec.Spec.Provider
		if err := resolveProv("spec.provider", "Secret", name, &sec.Spec.Provider); err != nil {
			return mutated, err
		}
		if sec.Spec.Provider != before {
			mutated.secrets = true
		}
	}
	for name, pol := range snap.policies {
		before := pol.Spec.Provider
		if err := resolveProv("spec.provider", "Policy", name, &pol.Spec.Provider); err != nil {
			return mutated, err
		}
		if pol.Spec.Provider != before {
			mutated.policies = true
		}
	}
	for name, m := range snap.models {
		before := m.Spec.Provider
		if err := resolveProv("spec.provider", "Model", name, &m.Spec.Provider); err != nil {
			return mutated, err
		}
		if m.Spec.Provider != before {
			mutated.models = true
		}
	}

	// Policy refs: ProviderSpec.DefaultPolicy and RelayKeySpec.PolicyRef.
	polIDBySlug := make(map[string]string, len(snap.policies))
	for slug, p := range snap.policies {
		if p.Metadata.ID == "" {
			continue
		}
		polIDBySlug[slug] = p.Metadata.ID
	}
	resolvePol := func(field, ownerKind, ownerName string, val *string) error {
		if *val == "" {
			return nil
		}
		if ids.Valid(*val) {
			return nil
		}
		id, ok := polIDBySlug[*val]
		if !ok {
			return fmt.Errorf("%s %q: %s references unknown policy %q",
				ownerKind, ownerName, field, *val)
		}
		*val = id
		return nil
	}

	for name, p := range snap.providers {
		before := p.Spec.DefaultPolicy
		if err := resolvePol("spec.defaultPolicy", "Provider", name, &p.Spec.DefaultPolicy); err != nil {
			return mutated, err
		}
		if p.Spec.DefaultPolicy != before {
			mutated.providers = true
		}
	}
	for name, k := range snap.relayKeys {
		before := k.Spec.PolicyRef
		if err := resolvePol("spec.policyRef", "RelayKey", name, &k.Spec.PolicyRef); err != nil {
			return mutated, err
		}
		if k.Spec.PolicyRef != before {
			mutated.relayKeys = true
		}
	}

	// Model refs: PolicySpec.Models[], RouteSpec.Models[], Deprecation.Replacement.
	// Models also accept Spec.Aliases as alternate human handles — accept either
	// the canonical slug or any alias on write (modelByName already does this).
	resolveModel := func(field, ownerKind, ownerName string, val *string) error {
		if *val == "" {
			return nil
		}
		if ids.Valid(*val) {
			return nil
		}
		m, ok := snap.modelByName(*val)
		if !ok {
			return fmt.Errorf("%s %q: %s references unknown model %q",
				ownerKind, ownerName, field, *val)
		}
		*val = m.Metadata.ID
		return nil
	}

	for name, pol := range snap.policies {
		for i := range pol.Spec.Models {
			before := pol.Spec.Models[i]
			if err := resolveModel("spec.models", "Policy", name, &pol.Spec.Models[i]); err != nil {
				return mutated, err
			}
			if pol.Spec.Models[i] != before {
				mutated.policies = true
			}
		}
	}
	for name, r := range snap.routes {
		for i := range r.Spec.Models {
			before := r.Spec.Models[i]
			if err := resolveModel("spec.models", "Route", name, &r.Spec.Models[i]); err != nil {
				return mutated, err
			}
			if r.Spec.Models[i] != before {
				mutated.routes = true
			}
		}
	}
	// Secret refs: PolicySpec.Secrets[].
	secIDBySlug := make(map[string]string, len(snap.secrets))
	for slug, sec := range snap.secrets {
		if sec.Metadata.ID == "" {
			continue
		}
		secIDBySlug[slug] = sec.Metadata.ID
	}
	resolveSecret := func(field, ownerKind, ownerName string, val *string) error {
		if *val == "" {
			return nil
		}
		if ids.Valid(*val) {
			return nil
		}
		id, ok := secIDBySlug[*val]
		if !ok {
			return fmt.Errorf("%s %q: %s references unknown secret %q",
				ownerKind, ownerName, field, *val)
		}
		*val = id
		return nil
	}
	for name, pol := range snap.policies {
		for i := range pol.Spec.Secrets {
			before := pol.Spec.Secrets[i]
			if err := resolveSecret("spec.secrets", "Policy", name, &pol.Spec.Secrets[i]); err != nil {
				return mutated, err
			}
			if pol.Spec.Secrets[i] != before {
				mutated.policies = true
			}
		}
	}

	for name, m := range snap.models {
		if m.Spec.Deprecation == nil {
			continue
		}
		before := m.Spec.Deprecation.Replacement
		if err := resolveModel("spec.deprecation.replacement", "Model", name, &m.Spec.Deprecation.Replacement); err != nil {
			return mutated, err
		}
		if m.Spec.Deprecation.Replacement != before {
			mutated.models = true
		}
	}

	// RateLimit attachments on Policy/Secret/Model.
	rlIDBySlug := make(map[string]string, len(snap.rateLimits))
	for slug, rl := range snap.rateLimits {
		if rl.Metadata.ID == "" {
			continue
		}
		rlIDBySlug[slug] = rl.Metadata.ID
	}
	resolveAttachments := func(ownerKind, ownerName string, attachments []RateLimitAttachment) (bool, error) {
		changed := false
		for i := range attachments {
			ref := attachments[i].Ref
			if ref == "" || ids.Valid(ref) {
				continue
			}
			id, ok := rlIDBySlug[ref]
			if !ok {
				return changed, fmt.Errorf("%s %q: rateLimits ref %q does not exist", ownerKind, ownerName, ref)
			}
			attachments[i].Ref = id
			changed = true
		}
		return changed, nil
	}
	for name, sec := range snap.secrets {
		if changed, err := resolveAttachments("Secret", name, sec.Spec.RateLimits); err != nil {
			return mutated, err
		} else if changed {
			mutated.secrets = true
		}
	}
	for name, pol := range snap.policies {
		if changed, err := resolveAttachments("Policy", name, pol.Spec.RateLimits); err != nil {
			return mutated, err
		} else if changed {
			mutated.policies = true
		}
	}
	for name, m := range snap.models {
		if changed, err := resolveAttachments("Model", name, m.Spec.RateLimits); err != nil {
			return mutated, err
		} else if changed {
			mutated.models = true
		}
	}

	return mutated, nil
}

// ProviderRefStore is the narrow view of a catalog needed by ResolveProviderRef.
// Implemented by *PGStore, *MemStore, and *YAMLStore.
type ProviderRefStore interface {
	ProviderByName(name string) (*Provider, bool)
	ProviderByID(id string) (*Provider, bool)
}

// ResolveProviderRef normalizes a provider reference (which may arrive as a
// name in admin POST/PUT bodies) to the canonical id form. Empty input is
// returned as-is — emptiness is rejected later by validate. Returns an error
// when the input matches neither a known id nor a known slug.
//
// Used by admin handlers at the write boundary so PG JSONB stores ids, not
// names. Mirrors what the snapshot-load resolver does, but for one ref at a
// time without requiring a full snapshot rebuild.
func ResolveProviderRef(store ProviderRefStore, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if ids.Valid(ref) {
		if _, ok := store.ProviderByID(ref); ok {
			return ref, nil
		}
		return "", fmt.Errorf("unknown provider id %q", ref)
	}
	if p, ok := store.ProviderByName(ref); ok {
		return p.Metadata.ID, nil
	}
	return "", fmt.Errorf("unknown provider %q", ref)
}

// PolicyRefStore is the narrow view needed by ResolvePolicyRef.
type PolicyRefStore interface {
	PolicyByName(name string) (*Policy, bool)
	PolicyByID(id string) (*Policy, bool)
}

// ResolvePolicyRef is the policy-side analogue of ResolveProviderRef.
func ResolvePolicyRef(store PolicyRefStore, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if ids.Valid(ref) {
		if _, ok := store.PolicyByID(ref); ok {
			return ref, nil
		}
		return "", fmt.Errorf("unknown policy id %q", ref)
	}
	if p, ok := store.PolicyByName(ref); ok {
		return p.Metadata.ID, nil
	}
	return "", fmt.Errorf("unknown policy %q", ref)
}

// RateLimitRefStore is the narrow view needed by ResolveRateLimitRef.
type RateLimitRefStore interface {
	RateLimitByName(name string) (*RateLimit, bool)
	RateLimitByID(id string) (*RateLimit, bool)
}

// ResolveRateLimitRef normalizes a rate-limit reference to its id.
func ResolveRateLimitRef(store RateLimitRefStore, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if ids.Valid(ref) {
		if _, ok := store.RateLimitByID(ref); ok {
			return ref, nil
		}
		return "", fmt.Errorf("unknown ratelimit id %q", ref)
	}
	if rl, ok := store.RateLimitByName(ref); ok {
		return rl.Metadata.ID, nil
	}
	return "", fmt.Errorf("unknown ratelimit %q", ref)
}

// SecretRefStore is the narrow view needed by ResolveSecretRef.
type SecretRefStore interface {
	SecretByName(name string) (*Secret, bool)
	SecretByID(id string) (*Secret, bool)
}

// ResolveSecretRef normalizes a secret reference to its id.
func ResolveSecretRef(store SecretRefStore, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if ids.Valid(ref) {
		if _, ok := store.SecretByID(ref); ok {
			return ref, nil
		}
		return "", fmt.Errorf("unknown secret id %q", ref)
	}
	if sec, ok := store.SecretByName(ref); ok {
		return sec.Metadata.ID, nil
	}
	return "", fmt.Errorf("unknown secret %q", ref)
}

// ModelRefStore is the narrow view needed by ResolveModelRef.
type ModelRefStore interface {
	ModelByName(name string) (*Model, bool)
	ModelByID(id string) (*Model, bool)
}

// ResolveModelRef normalizes a model reference (name, alias, or id) to its id.
// ModelByName already resolves aliases, so any human-known handle works.
func ResolveModelRef(store ModelRefStore, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if ids.Valid(ref) {
		if _, ok := store.ModelByID(ref); ok {
			return ref, nil
		}
		return "", fmt.Errorf("unknown model id %q", ref)
	}
	if m, ok := store.ModelByName(ref); ok {
		return m.Metadata.ID, nil
	}
	return "", fmt.Errorf("unknown model %q", ref)
}

// mutatedKinds reports which kinds had at least one row rewritten by
// resolveRefs. Used by PGStore.Reload to decide whether to write the rewrites
// back to Postgres (one-time self-healing migration).
type mutatedKinds struct {
	secrets   bool
	policies  bool
	models    bool
	providers bool
	relayKeys bool
	routes    bool
}

func (m mutatedKinds) any() bool {
	return m.secrets || m.policies || m.models || m.providers || m.relayKeys || m.routes
}

// finalizeIdentity is the standard snapshot-preparation sequence used by every
// loader (PG, YAML, Mem) and by tests that build a snapshot by hand: stamp
// any missing ids, build the id→slug index, and resolve cross-ref fields from
// name to id form. Callers run it before validate / injectUpstreamTier* /
// buildEffectivePricing.
func (s *snapshot) finalizeIdentity() error {
	ensureSnapshotIDs(s)
	s.buildByIDIndexes()
	if _, err := resolveRefs(s); err != nil {
		return err
	}
	return nil
}

// ensureSnapshotIDs stamps a fresh UUIDv7 on every row in the snapshot whose
// Metadata.ID is empty. Required precondition for resolveRefs, which needs a
// stable id on every row to build the slug→id lookup tables. Idempotent.
//
// PG-backed rows always carry their id from storage; this helper exists for
// the YAML and in-memory test paths where ids are not declared in fixtures.
func ensureSnapshotIDs(snap *snapshot) {
	for _, p := range snap.providers {
		if p.Metadata.ID == "" {
			p.Metadata.ID = ids.New()
		}
	}
	for _, p := range snap.policies {
		if p.Metadata.ID == "" {
			p.Metadata.ID = ids.New()
		}
	}
	for _, sec := range snap.secrets {
		if sec.Metadata.ID == "" {
			sec.Metadata.ID = ids.New()
		}
	}
	for _, m := range snap.models {
		if m.Metadata.ID == "" {
			m.Metadata.ID = ids.New()
		}
	}
	for _, r := range snap.routes {
		if r.Metadata.ID == "" {
			r.Metadata.ID = ids.New()
		}
	}
	for _, rl := range snap.rateLimits {
		if rl.Metadata.ID == "" {
			rl.Metadata.ID = ids.New()
		}
	}
	for _, k := range snap.relayKeys {
		if k.Metadata.ID == "" {
			k.Metadata.ID = ids.New()
		}
	}
}
