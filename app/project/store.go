// store.go is the data-access layer for Project. team_id is a real column
// (FK cascade) so deleting a Team drops its Projects without parsing JSONB.
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

// Store is the Project data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Project row.
func (s *Store) List(ctx context.Context) ([]*Project, error) {
	rows, err := s.q.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("project.List: %w", err)
	}
	out := make([]*Project, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r.ID, r.Name, r.DisplayName, r.TeamID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns the Project with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Project, error) {
	r, err := s.q.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("project.Get: %w", err)
	}
	return fromRow(r.ID, r.Name, r.DisplayName, r.TeamID, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
}

// Upsert writes p. Caller stamps Meta.ID; Owner is re-derived from TeamID.
func (s *Store) Upsert(ctx context.Context, p *Project) error {
	p.StampOwner()
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("project.Upsert: %w", err)
	}
	return s.q.UpsertProject(ctx, params)
}

// Delete removes a Project by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteProject(ctx, id)
}

func fromRow(id, name, displayName, teamID string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz) (*Project, error) {
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
	return gen.UpsertProjectParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		TeamID:      p.Spec.TeamID,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
