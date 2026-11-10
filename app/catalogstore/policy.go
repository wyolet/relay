// policy.go is the data-access layer for Policy. Spec.Models lives in its own
// column; PoolIDs, RateLimits and Budgets live in junction
// tables (policy_links.go), not in the JSONB spec column. Upsert fans out
// across `policies` and every junction inside a single transaction so
// callers see a consistent view.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// PolicyStore is the Policy data-access type. Holds a DB rather than Queries
// so Upsert can run a multi-table transaction; List reads without one.
type PolicyStore struct {
	db storage.DB
}

// NewPolicyStore constructs a PolicyStore bound to a pool or an open
// transaction.
func NewPolicyStore(db storage.DB) *PolicyStore { return &PolicyStore{db: db} }

// List returns every Policy row with its links.
func (s *PolicyStore) List(ctx context.Context) ([]*policy.Policy, error) {
	q := gen.New(s.db)
	rows, err := q.ListPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy.List: %w", err)
	}
	l, err := listLinks(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]*policy.Policy, 0, len(rows))
	for _, r := range rows {
		p, err := policyFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", r.Name, err)
		}
		p.Spec.PoolIDs = l.pools[p.Meta.ID]
		p.Spec.RateLimits = l.rateLimits[p.Meta.ID]
		p.Spec.Budgets = l.budgets[p.Meta.ID]
		p.Normalize()
		out = append(out, p)
	}
	return out, nil
}

// Get returns the Policy with the given id and its links, or (nil, nil) if
// not found.
func (s *PolicyStore) Get(ctx context.Context, id string) (*policy.Policy, error) {
	q := gen.New(s.db)
	r, err := q.GetPolicy(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("policy.Get: %w", err)
	}
	p, err := policyFromRow(gen.ListPoliciesRow(r))
	if err != nil {
		return nil, err
	}
	if err := getLinks(ctx, q, id, &p.Spec); err != nil {
		return nil, err
	}
	p.Normalize()
	return p, nil
}

// Upsert writes p across policies + junction tables in a single tx.
func (s *PolicyStore) Upsert(ctx context.Context, p *policy.Policy) error {
	params, err := policyUpsertParams(p)
	if err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policy.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := meta.StaleIfNoRows(q.UpsertPolicy(ctx, params)); err != nil {
		return fmt.Errorf("policy.Upsert: policies: %w", err)
	}
	if err := writeLinks(ctx, q, p); err != nil {
		return fmt.Errorf("policy.Upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a Policy by id. Junction rows cascade via FK.
func (s *PolicyStore) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeletePolicy(ctx, id)
}

func policyFromRow(r gen.ListPoliciesRow) (*policy.Policy, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec policy.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if len(r.Models) > 0 {
		if err := json.Unmarshal(r.Models, &spec.Models); err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
	}
	return &policy.Policy{Meta: md, Spec: spec}, nil
}

func policyUpsertParams(p *policy.Policy) (gen.UpsertPolicyParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("metadata: %w", err)
	}
	// The links and the model grant live in their own column and junctions.
	specCopy := policy.Spec{Enabled: p.Spec.Enabled}
	specJSON, err := json.Marshal(specCopy)
	if err != nil {
		return gen.UpsertPolicyParams{}, fmt.Errorf("spec: %w", err)
	}
	modelsJSON := []byte("[]")
	if len(p.Spec.Models) > 0 {
		modelsJSON, err = json.Marshal(p.Spec.Models)
		if err != nil {
			return gen.UpsertPolicyParams{}, fmt.Errorf("models: %w", err)
		}
	}
	ver, ok, err := p.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertPolicyParams{}, err
	}
	return gen.UpsertPolicyParams{
		ID:              p.Meta.ID,
		Name:            p.Meta.Name,
		DisplayName:     p.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		Models:          modelsJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
