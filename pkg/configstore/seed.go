package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/internal/db"
)

// SeedDiff holds the human-readable diff between a source ConfigStore and the current PG state.
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
func (s *PGStore) Diff(ctx context.Context, src ConfigStore) (*SeedDiff, error) {
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
func (s *PGStore) Seed(ctx context.Context, src ConfigStore) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("Seed: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)

	for _, p := range src.Providers() {
		meta, spec, err := marshalMetaSpec(p.Metadata, p.Spec)
		if err != nil {
			return fmt.Errorf("Seed: Provider %q: %w", p.Metadata.Name, err)
		}
		if err := q.UpsertProvider(ctx, db.UpsertProviderParams{Name: p.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertProvider %q: %w", p.Metadata.Name, err)
		}
	}

	for _, pool := range src.Pools() {
		meta, spec, err := marshalMetaSpec(pool.Metadata, pool.Spec)
		if err != nil {
			return fmt.Errorf("Seed: Pool %q: %w", pool.Metadata.Name, err)
		}
		if err := q.UpsertPool(ctx, db.UpsertPoolParams{Name: pool.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertPool %q: %w", pool.Metadata.Name, err)
		}
	}

	for _, sec := range src.Secrets() {
		meta, spec, err := marshalMetaSpec(sec.Metadata, sec.Spec)
		if err != nil {
			return fmt.Errorf("Seed: Secret %q: %w", sec.Metadata.Name, err)
		}
		if err := q.UpsertSecret(ctx, db.UpsertSecretParams{Name: sec.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertSecret %q: %w", sec.Metadata.Name, err)
		}
	}

	for _, m := range src.Models() {
		meta, spec, err := marshalMetaSpec(m.Metadata, m.Spec)
		if err != nil {
			return fmt.Errorf("Seed: Model %q: %w", m.Metadata.Name, err)
		}
		if err := q.UpsertModel(ctx, db.UpsertModelParams{Name: m.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertModel %q: %w", m.Metadata.Name, err)
		}
	}

	for _, r := range src.Routes() {
		meta, spec, err := marshalMetaSpec(r.Metadata, r.Spec)
		if err != nil {
			return fmt.Errorf("Seed: Route %q: %w", r.Metadata.Name, err)
		}
		if err := q.UpsertRoute(ctx, db.UpsertRouteParams{Name: r.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertRoute %q: %w", r.Metadata.Name, err)
		}
	}

	for _, rl := range src.RateLimits() {
		meta, spec, err := marshalMetaSpec(rl.Metadata, rl.Spec)
		if err != nil {
			return fmt.Errorf("Seed: RateLimit %q: %w", rl.Metadata.Name, err)
		}
		if err := q.UpsertRateLimit(ctx, db.UpsertRateLimitParams{Name: rl.Metadata.Name, Metadata: meta, Spec: spec}); err != nil {
			return fmt.Errorf("Seed: UpsertRateLimit %q: %w", rl.Metadata.Name, err)
		}
	}

	return tx.Commit(ctx)
}

// IsEmpty returns true when all catalog tables have zero rows.
func (s *PGStore) IsEmpty(ctx context.Context) (bool, error) {
	tables := []string{"providers", "pools", "secrets", "models", "routes", "rate_limits"}
	for _, t := range tables {
		var n int
		row := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+t)
		if err := row.Scan(&n); err != nil {
			return false, fmt.Errorf("IsEmpty: count %s: %w", t, err)
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}

func marshalMetaSpec(meta, spec any) ([]byte, []byte, error) {
	mb, err := json.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal metadata: %w", err)
	}
	sb, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal spec: %w", err)
	}
	return mb, sb, nil
}
