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

type Store struct {
	q *gen.Queries
}

func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

func (s *Store) List(ctx context.Context) ([]*Team, error) {
	rows, err := s.q.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("team.List: %w", err)
	}
	out := make([]*Team, 0, len(rows))
	for _, r := range rows {
		t, err := fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
		if err != nil {
			return nil, fmt.Errorf("team %s: %w", r.Name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Team, error) {
	r, err := s.q.GetTeam(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("team.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
}

// Upsert expects Meta.ID set by the caller.
func (s *Store) Upsert(ctx context.Context, t *Team) error {
	params, err := toUpsertParams(t)
	if err != nil {
		return fmt.Errorf("team.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertTeam(ctx, params))
}

// Delete relies on the FK to cascade the team's projects.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteTeam(ctx, id)
}

func fromRow(id, name, displayName string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz, version int64) (*Team, error) {
	md, err := meta.UnmarshalJSONB(id, name, displayName, metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = createdAt.Time
	md.UpdatedAt = updatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(version)
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
	ver, ok, err := t.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertTeamParams{}, err
	}
	return gen.UpsertTeamParams{
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
		ID:              t.Meta.ID,
		Name:            t.Meta.Name,
		DisplayName:     t.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
	}, nil
}
