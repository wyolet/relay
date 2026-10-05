// team_id is a real column (FK cascade) so deleting a Team drops its Projects
// without parsing JSONB.

package project

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

func (s *Store) List(ctx context.Context) ([]*Project, error) {
	rows, err := s.q.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("project.List: %w", err)
	}
	out := make([]*Project, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r.ID, r.Name, r.DisplayName, r.TeamID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Project, error) {
	r, err := s.q.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("project.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.TeamID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
}

// Upsert expects Meta.ID set by the caller and re-derives Owner from TeamID.
func (s *Store) Upsert(ctx context.Context, p *Project) error {
	p.StampOwner()
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("project.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertProject(ctx, params))
}

func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteProject(ctx, id)
}

func fromRow(id, name, displayName, teamID string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz, version int64) (*Project, error) {
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
	sp.TeamID = teamID
	p := &Project{Meta: md, Spec: sp}
	p.StampOwner()
	return p, nil
}

func toUpsertParams(p *Project) (gen.UpsertProjectParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertProjectParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return gen.UpsertProjectParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := p.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertProjectParams{}, err
	}
	return gen.UpsertProjectParams{
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
		ID:              p.Meta.ID,
		Name:            p.Meta.Name,
		DisplayName:     p.Meta.DisplayName,
		TeamID:          p.Spec.TeamID,
		Metadata:        metaJSON,
		Spec:            specJSON,
	}, nil
}
