package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Budget data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Budget row.
func (s *Store) List(ctx context.Context) ([]*Budget, error) {
	rows, err := s.q.ListBudgets(ctx)
	if err != nil {
		return nil, fmt.Errorf("budget.List: %w", err)
	}
	out := make([]*Budget, 0, len(rows))
	for _, r := range rows {
		b, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("budget %s: %w", r.Name, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Get returns the Budget with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Budget, error) {
	r, err := s.q.GetBudget(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("budget.Get: %w", err)
	}
	return fromRow(r)
}

// Upsert writes b. The caller sets Meta.ID.
func (s *Store) Upsert(ctx context.Context, b *Budget) error {
	params, err := toUpsertParams(b)
	if err != nil {
		return fmt.Errorf("budget.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertBudget(ctx, params))
}

// Delete removes a Budget by id; policies linking it lose the link.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteBudget(ctx, id)
}

func fromRow(r gen.Budget) (*Budget, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Budget{Meta: md, Spec: spec}, nil
}

func toUpsertParams(b *Budget) (gen.UpsertBudgetParams, error) {
	metaJSON, err := meta.MarshalJSONB(b.Meta)
	if err != nil {
		return gen.UpsertBudgetParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(b.Spec)
	if err != nil {
		return gen.UpsertBudgetParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := b.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertBudgetParams{}, err
	}
	return gen.UpsertBudgetParams{
		ID:              b.Meta.ID,
		Name:            b.Meta.Name,
		DisplayName:     b.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
