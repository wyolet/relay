// store.go is the data-access layer for Model. Mirrors app/provider/store.go;
// metadata JSONB encoding is delegated to app/meta.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Model data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Model row.
func (s *Store) List(ctx context.Context) ([]*Model, error) {
	rows, err := s.q.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("model.List: %w", err)
	}
	out := make([]*Model, 0, len(rows))
	for _, r := range rows {
		m, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", r.Name, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Upsert writes m. Caller is responsible for stamping Meta.ID.
func (s *Store) Upsert(ctx context.Context, m *Model) error {
	params, err := toUpsertParams(m)
	if err != nil {
		return fmt.Errorf("model.Upsert: %w", err)
	}
	return s.q.UpsertModel(ctx, params)
}

// Get returns the Model with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Model, error) {
	r, err := s.q.GetModel(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("model.Get: %w", err)
	}
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Model{Meta: m, Spec: spec}, nil
}

// Delete removes a Model by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteModel(ctx, id)
}

func fromRow(r gen.ListModelsRow) (*Model, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Model{Meta: md, Spec: spec}, nil
}

func toUpsertParams(m *Model) (gen.UpsertModelParams, error) {
	metaJSON, err := meta.MarshalJSONB(m.Meta)
	if err != nil {
		return gen.UpsertModelParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(m.Spec)
	if err != nil {
		return gen.UpsertModelParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertModelParams{
		ID:          m.Meta.ID,
		Name:        m.Meta.Name,
		DisplayName: m.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
