package catalog

import (
	"context"
	"fmt"
	"sort"

	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/ids"
)

// ensureID stamps a fresh UUIDv7 on meta.ID when empty. Use existing[slug]
// to preserve the id of a row that already exists in PG (re-seed idempotency);
// pass nil when the table is known empty.
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
// Validation must pass before calling; if it fails, no transaction is opened.
func (s *PGStore) Seed(ctx context.Context, src Store) error {
	// Build slug→id maps for each kind from the existing PG state so re-seed
	// preserves ids (idempotent across runs). On a TRUNCATEd table the maps
	// are empty and ensureID stamps fresh UUIDv7s.
	existing, err := s.collectExistingIDs(ctx)
	if err != nil {
		return fmt.Errorf("Seed: collect existing ids: %w", err)
	}
	return s.tx.WithTxCatalog(ctx, func(db CatalogDB) error {
		for _, p := range src.Providers() {
			ensureID(&p.Metadata, existing.providers)
			if err := db.UpsertProvider(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertProvider %q: %w", p.Metadata.Name, err)
			}
		}
		for _, p := range src.Policies() {
			ensureID(&p.Metadata, existing.policies)
			if err := db.UpsertPolicy(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertPolicy %q: %w", p.Metadata.Name, err)
			}
		}
		for _, sec := range src.Secrets() {
			ensureID(&sec.Metadata, existing.secrets)
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
			ensureID(&m.Metadata, existing.models)
			if err := db.UpsertModel(ctx, *m); err != nil {
				return fmt.Errorf("Seed: UpsertModel %q: %w", m.Metadata.Name, err)
			}
		}
		for _, r := range src.Routes() {
			ensureID(&r.Metadata, existing.routes)
			if err := db.UpsertRoute(ctx, *r); err != nil {
				return fmt.Errorf("Seed: UpsertRoute %q: %w", r.Metadata.Name, err)
			}
		}
		for _, rl := range src.RateLimits() {
			ensureID(&rl.Metadata, existing.rateLimits)
			if err := db.UpsertRateLimit(ctx, *rl); err != nil {
				return fmt.Errorf("Seed: UpsertRateLimit %q: %w", rl.Metadata.Name, err)
			}
		}
		return nil
	})
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
