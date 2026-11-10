// model.go is the data-access layer for Model. Mirrors provider.go; metadata
// JSONB encoding is delegated to pkg/meta.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
)

// ModelStore is the Model data-access type.
type ModelStore struct {
	q *gen.Queries
}

// NewModelStore constructs a ModelStore from an existing sqlc Queries handle.
func NewModelStore(q *gen.Queries) *ModelStore { return &ModelStore{q: q} }

// List returns every Model row.
func (s *ModelStore) List(ctx context.Context) ([]*model.Model, error) {
	rows, err := s.q.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("model.List: %w", err)
	}
	out := make([]*model.Model, 0, len(rows))
	for _, r := range rows {
		m, err := modelFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", r.Name, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Upsert writes m. Caller is responsible for stamping Meta.ID.
func (s *ModelStore) Upsert(ctx context.Context, m *model.Model) error {
	params, err := modelUpsertParams(m)
	if err != nil {
		return fmt.Errorf("model.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertModel(ctx, params))
}

// Get returns the Model with the given id, or (nil, nil) if not found.
func (s *ModelStore) Get(ctx context.Context, id string) (*model.Model, error) {
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
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec model.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &model.Model{Meta: m, Spec: spec}, nil
}

// Delete removes a Model by id.
func (s *ModelStore) Delete(ctx context.Context, id string) error {
	return s.q.DeleteModel(ctx, id)
}

func modelFromRow(r gen.ListModelsRow) (*model.Model, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec model.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &model.Model{Meta: md, Spec: spec}, nil
}

func modelUpsertParams(m *model.Model) (gen.UpsertModelParams, error) {
	metaJSON, err := meta.MarshalJSONB(m.Meta)
	if err != nil {
		return gen.UpsertModelParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(m.Spec)
	if err != nil {
		return gen.UpsertModelParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := m.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertModelParams{}, err
	}
	return gen.UpsertModelParams{
		ID:              m.Meta.ID,
		Name:            m.Meta.Name,
		DisplayName:     m.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
