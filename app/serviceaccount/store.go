// store.go is the data-access layer for ServiceAccount. project_id is a
// real column (FK cascade) so deleting a Project drops its accounts
// without parsing JSONB.
package serviceaccount

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

// Store is the ServiceAccount data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every ServiceAccount row.
func (s *Store) List(ctx context.Context) ([]*ServiceAccount, error) {
	rows, err := s.q.ListServiceAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("serviceaccount.List: %w", err)
	}
	out := make([]*ServiceAccount, 0, len(rows))
	for _, r := range rows {
		sa, err := fromRow(r.ID, r.Name, r.DisplayName, r.ProjectID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("serviceaccount %s: %w", r.Name, err)
		}
		out = append(out, sa)
	}
	return out, nil
}

// Get returns the ServiceAccount with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*ServiceAccount, error) {
	r, err := s.q.GetServiceAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("serviceaccount.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.ProjectID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
}

// Upsert writes sa. Caller stamps Meta.ID; Owner is re-derived from ProjectID.
func (s *Store) Upsert(ctx context.Context, sa *ServiceAccount) error {
	sa.StampOwner()
	params, err := toUpsertParams(sa)
	if err != nil {
		return fmt.Errorf("serviceaccount.Upsert: %w", err)
	}
	return s.q.UpsertServiceAccount(ctx, params)
}

// Delete removes a ServiceAccount by id. Keys cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteServiceAccount(ctx, id)
}

func fromRow(id, name, displayName, projectID string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz) (*ServiceAccount, error) {
	md, err := meta.UnmarshalJSONB(id, name, displayName, metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = createdAt.Time
	md.UpdatedAt = updatedAt.Time
	var sp Spec
	if err := json.Unmarshal(spec, &sp); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	sp.ProjectID = projectID
	sa := &ServiceAccount{Meta: md, Spec: sp}
	sa.StampOwner()
	return sa, nil
}

func toUpsertParams(sa *ServiceAccount) (gen.UpsertServiceAccountParams, error) {
	metaJSON, err := meta.MarshalJSONB(sa.Meta)
	if err != nil {
		return gen.UpsertServiceAccountParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(sa.Spec)
	if err != nil {
		return gen.UpsertServiceAccountParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertServiceAccountParams{
		ID:          sa.Meta.ID,
		Name:        sa.Meta.Name,
		DisplayName: sa.Meta.DisplayName,
		ProjectID:   sa.Spec.ProjectID,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
