// Projects are stored by the auth module's directory store; team_id is a real column there (FK cascade), so deleting a Team drops its Projects without parsing JSONB.

package project

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.Project]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).Projects} }

// Directory is the auth module's store under this one, for writes that run through the directory.
func (s *Store) Directory() directory.Store[directory.Project] { return s.rows }

func (s *Store) List(ctx context.Context) ([]*Project, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("project.List: %w", err)
	}
	out := make([]*Project, 0, len(rows))
	for _, r := range rows {
		p, err := FromDirectory(r)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", r.Meta.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Project, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("project.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r)
}

// Upsert expects Meta.ID set by the caller and re-derives Owner from TeamID.
func (s *Store) Upsert(ctx context.Context, p *Project) error {
	p.StampOwner()
	r, err := p.ToDirectory()
	if err != nil {
		return fmt.Errorf("project.Upsert: %w", err)
	}
	return meta.FromDirectoryError(s.rows.Upsert(ctx, r))
}

func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
