package catalog

import (
	"context"
	"fmt"
	"sort"

	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/ids"
)

// ensureID stamps a fresh UUIDv7 on meta.ID when empty. Seed no longer uses
// it (Seed builds its own name→id index via buildSeedIndex); kept for any
// remaining callers that need a no-op-when-present id stamp.
func ensureID(meta *Metadata, existing map[string]string) {
	if meta.ID != "" {
		return
	}
	if existing != nil {
		if id, ok := existing[meta.Name]; ok {
			meta.ID = id
			return
		}
	}
	meta.ID = ids.New()
}

// SeedDiff holds the human-readable diff between a source Store and the current PG state.
type SeedDiff struct {
	Providers  KindDiff
	Policies      KindDiff
	Secrets    KindDiff
	Models     KindDiff
	Routes     KindDiff
	RateLimits KindDiff
}

// KindDiff lists create/update/delete names for a single resource kind.
type KindDiff struct {
	Kind    string
	Creates []string
	Updates []string
	Deletes []string
}

// Empty reports whether this kind has no changes.
func (d KindDiff) Empty() bool {
	return len(d.Creates) == 0 && len(d.Updates) == 0 && len(d.Deletes) == 0
}

// Empty reports whether the diff contains no changes.
func (d *SeedDiff) Empty() bool {
	return d.Providers.Empty() && d.Policies.Empty() && d.Secrets.Empty() &&
		d.Models.Empty() && d.Routes.Empty() && d.RateLimits.Empty()
}

// Diff computes the diff between src and the current PG state.
func (s *PGStore) Diff(ctx context.Context, src Store) (*SeedDiff, error) {
	pgSnap, err := s.loadSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("Diff: load pg snapshot: %w", err)
	}

	diff := &SeedDiff{}
	diff.Providers = diffNames("Provider", pgSnap.providers, src.Providers(), func(v *Provider) string { return v.Metadata.Name })
	diff.Policies = diffNames("Policy", pgSnap.policies, src.Policies(), func(v *Policy) string { return v.Metadata.Name })
	diff.Secrets = diffNames("Secret", pgSnap.secrets, src.Secrets(), func(v *Secret) string { return v.Metadata.Name })
	diff.Models = diffNames("Model", pgSnap.models, src.Models(), func(v *Model) string { return v.Metadata.Name })
	diff.Routes = diffNames("Route", pgSnap.routes, src.Routes(), func(v *Route) string { return v.Metadata.Name })
	diff.RateLimits = diffNames("RateLimit", pgSnap.rateLimits, src.RateLimits(), func(v *RateLimit) string { return v.Metadata.Name })
	return diff, nil
}

func diffNames[T any, M map[string]T](kind string, pgMap M, srcList []T, name func(T) string) KindDiff {
	d := KindDiff{Kind: kind}
	for _, v := range srcList {
		n := name(v)
		if _, ok := pgMap[n]; ok {
			d.Updates = append(d.Updates, n)
		} else {
			d.Creates = append(d.Creates, n)
		}
	}
	srcSet := map[string]struct{}{}
	for _, v := range srcList {
		srcSet[name(v)] = struct{}{}
	}
	for n := range pgMap {
		if _, ok := srcSet[n]; !ok {
			d.Deletes = append(d.Deletes, n)
		}
	}
	sort.Strings(d.Creates)
	sort.Strings(d.Updates)
	sort.Strings(d.Deletes)
	return d
}

// Seed upserts src into Postgres in a single transaction.
//
// Contract: src holds rows in *name form* — cross-ref Spec fields contain
// slugs, not ids. Use catalog.LoadYAMLForSeed to produce such a source.
//
// Seed builds one name→id index per kind by checking PG for an existing row
// of the same name (preserves id across re-seeds) and stamping a fresh
// UUIDv7 otherwise. It then applies the index uniformly: stamps each row's
// Metadata.ID and rewrites every Spec ref field from name to id. After
// validation the rows are upserted in a single transaction.
//
// This replaces the previous approach (load YAML through the snapshot
// resolver, then try to reconcile against PG). The resolver stamped random
// UUIDs for each YAML row on every load; on re-seed against a populated PG
// those would diverge from PG's canonical ids and leave Spec refs pointing
// at non-existent provider/policy/etc. rows.
func (s *PGStore) Seed(ctx context.Context, src Store) error {
	existing, err := s.collectExistingIDs(ctx)
	if err != nil {
		return fmt.Errorf("Seed: collect existing ids: %w", err)
	}

	idx := buildSeedIndex(src, existing)
	if err := idx.applyTo(src); err != nil {
		return fmt.Errorf("Seed: resolve refs: %w", err)
	}
	if ys, ok := src.(*YAMLStore); ok {
		ys.snap.buildByIDIndexes()
		if err := validate(ys.snap); err != nil {
			return fmt.Errorf("Seed: catalog invalid: %w", err)
		}
	}

	return s.tx.WithTxCatalog(ctx, func(db CatalogDB) error {
		for _, p := range src.Providers() {
			if err := db.UpsertProvider(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertProvider %q: %w", p.Metadata.Name, err)
			}
		}
		for _, p := range src.Policies() {
			if err := db.UpsertPolicy(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertPolicy %q: %w", p.Metadata.Name, err)
			}
		}
		for _, sec := range src.Secrets() {
			switch {
			case sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "":
				if err := db.UpsertSecretEnv(ctx, sec.Spec.ValueFrom.Env, sec.Spec.Provider, sec.Metadata); err != nil {
					return fmt.Errorf("Seed: UpsertSecret %q: %w", sec.Metadata.Name, err)
				}
			case sec.Spec.Value != "":
				if len(s.masterKey) == 0 {
					return fmt.Errorf("Seed: secret %q uses stored value but RELAY_MASTER_KEY not set", sec.Metadata.Name)
				}
				ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(sec.Spec.Value))
				if err != nil {
					return fmt.Errorf("Seed: secret %q: encrypt: %w", sec.Metadata.Name, err)
				}
				if err := db.UpsertSecretStored(ctx, sec.Spec.Provider, sec.Metadata, ct, nonce); err != nil {
					return fmt.Errorf("Seed: UpsertSecret %q: %w", sec.Metadata.Name, err)
				}
			default:
				return fmt.Errorf("Seed: secret %q has no valueFrom.env or value", sec.Metadata.Name)
			}
		}
		for _, m := range src.Models() {
			if err := db.UpsertModel(ctx, *m); err != nil {
				return fmt.Errorf("Seed: UpsertModel %q: %w", m.Metadata.Name, err)
			}
		}
		for _, r := range src.Routes() {
			if err := db.UpsertRoute(ctx, *r); err != nil {
				return fmt.Errorf("Seed: UpsertRoute %q: %w", r.Metadata.Name, err)
			}
		}
		for _, rl := range src.RateLimits() {
			if err := db.UpsertRateLimit(ctx, *rl); err != nil {
				return fmt.Errorf("Seed: UpsertRateLimit %q: %w", rl.Metadata.Name, err)
			}
		}
		return nil
	})
}

// seedIndex is the name→id table Seed builds before writing.
// One sub-map per kind. Ids come from existing PG rows when the name matches,
// otherwise a fresh UUIDv7 is stamped.
type seedIndex struct {
	providers, policies, secrets, models, routes, rateLimits map[string]string
}

func buildSeedIndex(src Store, existing *existingIDs) *seedIndex {
	idx := &seedIndex{
		providers:  map[string]string{},
		policies:   map[string]string{},
		secrets:    map[string]string{},
		models:     map[string]string{},
		routes:     map[string]string{},
		rateLimits: map[string]string{},
	}
	assign := func(out, ex map[string]string, name string) string {
		if id, ok := ex[name]; ok {
			out[name] = id
			return id
		}
		id := ids.New()
		out[name] = id
		return id
	}
	for _, p := range src.Providers() {
		assign(idx.providers, existing.providers, p.Metadata.Name)
	}
	for _, p := range src.Policies() {
		assign(idx.policies, existing.policies, p.Metadata.Name)
	}
	for _, sec := range src.Secrets() {
		assign(idx.secrets, existing.secrets, sec.Metadata.Name)
	}
	for _, m := range src.Models() {
		assign(idx.models, existing.models, m.Metadata.Name)
	}
	for _, r := range src.Routes() {
		assign(idx.routes, existing.routes, r.Metadata.Name)
	}
	for _, rl := range src.RateLimits() {
		assign(idx.rateLimits, existing.rateLimits, rl.Metadata.Name)
	}
	return idx
}

// applyTo stamps Metadata.ID on every row and rewrites every cross-ref Spec
// field from name (slug) form to id form via the index. Returns an error if
// any ref names a resource the index doesn't know.
func (idx *seedIndex) applyTo(src Store) error {
	lookup := func(kind, parent, field, mapName string, m map[string]string, val *string) error {
		if *val == "" {
			return nil
		}
		id, ok := m[*val]
		if !ok {
			return fmt.Errorf("%s %q: %s references unknown %s %q", kind, parent, field, mapName, *val)
		}
		*val = id
		return nil
	}

	for _, p := range src.Providers() {
		p.Metadata.ID = idx.providers[p.Metadata.Name]
		if err := lookup("Provider", p.Metadata.Name, "spec.defaultPolicy", "policy", idx.policies, &p.Spec.DefaultPolicy); err != nil {
			return err
		}
	}
	for _, p := range src.Policies() {
		p.Metadata.ID = idx.policies[p.Metadata.Name]
		if err := lookup("Policy", p.Metadata.Name, "spec.provider", "provider", idx.providers, &p.Spec.Provider); err != nil {
			return err
		}
		for i := range p.Spec.Secrets {
			if err := lookup("Policy", p.Metadata.Name, "spec.secrets", "secret", idx.secrets, &p.Spec.Secrets[i]); err != nil {
				return err
			}
		}
		for i := range p.Spec.Models {
			if err := lookup("Policy", p.Metadata.Name, "spec.models", "model", idx.models, &p.Spec.Models[i]); err != nil {
				return err
			}
		}
		for i := range p.Spec.RateLimits {
			if err := lookup("Policy", p.Metadata.Name, "spec.rateLimits", "ratelimit", idx.rateLimits, &p.Spec.RateLimits[i].Ref); err != nil {
				return err
			}
		}
	}
	for _, sec := range src.Secrets() {
		sec.Metadata.ID = idx.secrets[sec.Metadata.Name]
		if err := lookup("Secret", sec.Metadata.Name, "spec.provider", "provider", idx.providers, &sec.Spec.Provider); err != nil {
			return err
		}
		for i := range sec.Spec.RateLimits {
			if err := lookup("Secret", sec.Metadata.Name, "spec.rateLimits", "ratelimit", idx.rateLimits, &sec.Spec.RateLimits[i].Ref); err != nil {
				return err
			}
		}
	}
	for _, m := range src.Models() {
		m.Metadata.ID = idx.models[m.Metadata.Name]
		if err := lookup("Model", m.Metadata.Name, "spec.provider", "provider", idx.providers, &m.Spec.Provider); err != nil {
			return err
		}
		if m.Spec.Deprecation != nil {
			if err := lookup("Model", m.Metadata.Name, "spec.deprecation.replacement", "model", idx.models, &m.Spec.Deprecation.Replacement); err != nil {
				return err
			}
		}
		for i := range m.Spec.RateLimits {
			if err := lookup("Model", m.Metadata.Name, "spec.rateLimits", "ratelimit", idx.rateLimits, &m.Spec.RateLimits[i].Ref); err != nil {
				return err
			}
		}
	}
	for _, r := range src.Routes() {
		r.Metadata.ID = idx.routes[r.Metadata.Name]
		for i := range r.Spec.Models {
			if err := lookup("Route", r.Metadata.Name, "spec.models", "model", idx.models, &r.Spec.Models[i]); err != nil {
				return err
			}
		}
	}
	for _, rl := range src.RateLimits() {
		rl.Metadata.ID = idx.rateLimits[rl.Metadata.Name]
	}
	return nil
}

// existingIDs holds slug→id maps used by Seed to keep re-runs idempotent.
type existingIDs struct {
	providers, policies, secrets, models, routes, rateLimits map[string]string
}

func (s *PGStore) collectExistingIDs(ctx context.Context) (*existingIDs, error) {
	out := &existingIDs{
		providers:  map[string]string{},
		policies:   map[string]string{},
		secrets:    map[string]string{},
		models:     map[string]string{},
		routes:     map[string]string{},
		rateLimits: map[string]string{},
	}
	provs, err := s.db.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range provs {
		out.providers[p.Metadata.Name] = p.Metadata.ID
	}
	pols, err := s.db.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pols {
		out.policies[p.Metadata.Name] = p.Metadata.ID
	}
	secs, err := s.db.ListSecretRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, sec := range secs {
		out.secrets[sec.Name] = sec.ID
	}
	mods, err := s.db.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		out.models[m.Metadata.Name] = m.Metadata.ID
	}
	rts, err := s.db.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rts {
		out.routes[r.Metadata.Name] = r.Metadata.ID
	}
	rls, err := s.db.ListRateLimits(ctx)
	if err != nil {
		return nil, err
	}
	for _, rl := range rls {
		out.rateLimits[rl.Metadata.Name] = rl.Metadata.ID
	}
	return out, nil
}

// IsEmpty returns true when all catalog tables have zero rows.
func (s *PGStore) IsEmpty(ctx context.Context) (bool, error) {
	return s.db.IsEmpty(ctx)
}
