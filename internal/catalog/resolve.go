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
// Currently handles Provider refs only:
//   - SecretSpec.Provider
//   - PolicySpec.Provider
//   - ModelSpec.Provider
//
// Other cross-refs (Policy refs, Model refs, Secret refs, RateLimit
// attachments) will land in subsequent PRs following this same pattern.
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

// mutatedKinds reports which kinds had at least one row rewritten by
// resolveRefs. Used by PGStore.Reload to decide whether to write the rewrites
// back to Postgres (one-time self-healing migration).
type mutatedKinds struct {
	secrets  bool
	policies bool
	models   bool
}

func (m mutatedKinds) any() bool {
	return m.secrets || m.policies || m.models
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
