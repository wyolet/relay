// store.go is the data-access layer for Policy. Spec.ModelIDs,
// Spec.HostKeyIDs, and Spec.RateLimitID live in their dedicated columns /
// junction tables (migration 0009) — not in the JSONB spec column.
// Upsert fans out across `policies`, `policy_models`, and
// `policy_host_keys` inside a single transaction so callers see a
// consistent view.
package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Policy data-access type. Holds a pool so Upsert can run a
// multi-table transaction; List uses the pool directly without one.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store bound to a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns every Policy row, hydrating ModelIDs/HostKeyIDs/RateLimitID
// from their relational sources.
func (s *Store) List(ctx context.Context) ([]*Policy, error) {
	q := gen.New(s.pool)
	rows, err := q.ListPoliciesWithRateLimit(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List: %w", err)
	}
	modelRows, err := q.ListPolicyModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List models: %w", err)
	}
	keyRows, err := q.ListPolicyHostKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List hostKeys: %w", err)
	}

	modelsByPolicy := map[string][]string{}
	for _, r := range modelRows {
		modelsByPolicy[r.PolicyID] = append(modelsByPolicy[r.PolicyID], r.ModelID)
	}
	keysByPolicy := map[string][]string{}
	for _, r := range keyRows {
		keysByPolicy[r.PolicyID] = append(keysByPolicy[r.PolicyID], r.HostKeyID)
	}

	out := make([]*Policy, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", r.Name, err)
		}
		p.Spec.ModelIDs = modelsByPolicy[p.Meta.ID]
		p.Spec.HostKeyIDs = keysByPolicy[p.Meta.ID]
		out = append(out, p)
	}
	return out, nil
}

// Upsert writes p across policies + junction tables in a single tx.
func (s *Store) Upsert(ctx context.Context, p *Policy) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policy.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := q.UpsertPolicy(ctx, params); err != nil {
		return fmt.Errorf("policy.Upsert: policies: %w", err)
	}
	rateLimitID := pgtype.Text{}
	if p.Spec.RateLimitID != "" {
		rateLimitID = pgtype.Text{String: p.Spec.RateLimitID, Valid: true}
	}
	if err := q.SetPolicyRateLimit(ctx, gen.SetPolicyRateLimitParams{
		ID:          p.Meta.ID,
		RateLimitID: rateLimitID,
	}); err != nil {
		return fmt.Errorf("policy.Upsert: rate_limit_id: %w", err)
	}
	if err := q.DeletePolicyModels(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("policy.Upsert: clear models: %w", err)
	}
	for i, id := range p.Spec.ModelIDs {
		if err := q.InsertPolicyModel(ctx, gen.InsertPolicyModelParams{
			PolicyID: p.Meta.ID,
			ModelID:  id,
			Position: int32(i),
		}); err != nil {
			return fmt.Errorf("policy.Upsert: insert model %s: %w", id, err)
		}
	}
	if err := q.DeletePolicyHostKeys(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("policy.Upsert: clear hostKeys: %w", err)
	}
	for i, id := range p.Spec.HostKeyIDs {
		if err := q.InsertPolicyHostKey(ctx, gen.InsertPolicyHostKeyParams{
			PolicyID:  p.Meta.ID,
			HostKeyID: id,
			Position:  int32(i),
		}); err != nil {
			return fmt.Errorf("policy.Upsert: insert hostKey %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a Policy by id. Junction rows cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.pool).DeletePolicy(ctx, id)
}

func fromRow(r gen.ListPoliciesWithRateLimitRow) (*Policy, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if r.RateLimitID.Valid {
		spec.RateLimitID = r.RateLimitID.String
	}
	return &Policy{Meta: md, Spec: spec}, nil
}

func toUpsertParams(p *Policy) (gen.UpsertPolicyParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Strip relational fields from the JSONB spec — they live in columns / junctions now.
	specCopy := p.Spec
	specCopy.ModelIDs = nil
	specCopy.HostKeyIDs = nil
	specCopy.RateLimitID = ""
	specJSON, err := json.Marshal(specCopy)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertPolicyParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
