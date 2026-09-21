// store.go is the data-access layer for Team. Same shape as the other
// entity stores.
package team

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

// Store is the Team data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Team row.
func (s *Store) List(ctx context.Context) ([]*Team, error) {
	rows, err := s.q.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("team.List: %w", err)
	}
	out := make([]*Team, 0, len(rows))
	for _, r := range rows {
		t, err := fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("team %s: %w", r.Name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Get returns the Team with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Team, error) {
	r, err := s.q.GetTeam(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("team.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
}

// Upsert writes t. Caller stamps Meta.ID.
func (s *Store) Upsert(ctx context.Context, t *Team) error {
	params, err := toUpsertParams(t)
	if err != nil {
		return fmt.Errorf("team.Upsert: %w", err)
	}
	return s.q.UpsertTeam(ctx, params)
}

// Delete removes a Team by id. Projects cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteTeam(ctx, id)
}

func fromRow(id, name, displayName string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz) (*Team, error) {
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
	return &Team{Meta: md, Spec: sp}, nil
}

func toUpsertParams(t *Team) (gen.UpsertTeamParams, error) {
	metaJSON, err := meta.MarshalJSONB(t.Meta)
	if err != nil {
		return gen.UpsertTeamParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(t.Spec)
	if err != nil {
		return gen.UpsertTeamParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertTeamParams{
		ID:          t.Meta.ID,
		Name:        t.Meta.Name,
		DisplayName: t.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
