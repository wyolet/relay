package catalog

import (
	"context"
	"fmt"
	"sort"

	"github.com/wyolet/relay/pkg/crypto"
)

// SeedDiff holds the human-readable diff between a source Store and the current PG state.
type SeedDiff struct {
	Providers  KindDiff
	Pools      KindDiff
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
	return d.Providers.Empty() && d.Pools.Empty() && d.Secrets.Empty() &&
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
	diff.Pools = diffNames("Pool", pgSnap.pools, src.Pools(), func(v *Pool) string { return v.Metadata.Name })
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
	return s.tx.WithTxCatalog(ctx, func(db CatalogDB) error {
		for _, p := range src.Providers() {
			if err := db.UpsertProvider(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertProvider %q: %w", p.Metadata.Name, err)
			}
		}
		for _, p := range src.Pools() {
			if err := db.UpsertPool(ctx, *p); err != nil {
				return fmt.Errorf("Seed: UpsertPool %q: %w", p.Metadata.Name, err)
			}
		}
		for _, sec := range src.Secrets() {
			switch {
			case sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "":
				if err := db.UpsertSecretEnv(ctx, sec.Metadata.Name, sec.Spec.ValueFrom.Env, sec.Spec.Provider, sec.Metadata); err != nil {
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
				if err := db.UpsertSecretStored(ctx, sec.Metadata.Name, sec.Spec.Provider, sec.Metadata, ct, nonce); err != nil {
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

// IsEmpty returns true when all catalog tables have zero rows.
func (s *PGStore) IsEmpty(ctx context.Context) (bool, error) {
	return s.db.IsEmpty(ctx)
}
