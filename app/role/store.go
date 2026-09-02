// store.go is the data-access layer for Role. Rules live in the spec JSONB;
// a Role has no relational columns of its own.
package role

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

// Store is the Role data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Role row.
func (s *Store) List(ctx context.Context) ([]*Role, error) {
	rows, err := s.q.ListRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("role.List: %w", err)
	}
	out := make([]*Role, 0, len(rows))
	for _, r := range rows {
		role, err := fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("role %s: %w", r.Name, err)
		}
		out = append(out, role)
	}
	return out, nil
}

// Get returns the Role with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Role, error) {
	r, err := s.q.GetRole(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("role.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
}

// Upsert writes r. Caller stamps Meta.ID.
func (s *Store) Upsert(ctx context.Context, r *Role) error {
	params, err := toUpsertParams(r)
	if err != nil {
		return fmt.Errorf("role.Upsert: %w", err)
	}
	return s.q.UpsertRole(ctx, params)
}

// Delete removes a Role by id. Its bindings cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteRole(ctx, id)
}

func fromRow(id, name, displayName string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz) (*Role, error) {
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
	return &Role{Meta: md, Spec: sp}, nil
}

func toUpsertParams(r *Role) (gen.UpsertRoleParams, error) {
	metaJSON, err := meta.MarshalJSONB(r.Meta)
	if err != nil {
		return gen.UpsertRoleParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(r.Spec)
	if err != nil {
		return gen.UpsertRoleParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertRoleParams{
		ID:          r.Meta.ID,
		Name:        r.Meta.Name,
		DisplayName: r.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
