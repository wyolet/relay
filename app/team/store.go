// Teams are stored by the auth module's directory store; this adapts its rows to the relay's Team.

package team

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.Team]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).Teams} }

// Directory is the auth module's store under this one, for writes that run through the directory.
func (s *Store) Directory() directory.Store[directory.Team] { return s.rows }

func (s *Store) List(ctx context.Context) ([]*Team, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("team.List: %w", err)
	}
	out := make([]*Team, 0, len(rows))
	for _, r := range rows {
		t, err := FromDirectory(r)
		if err != nil {
			return nil, fmt.Errorf("team %s: %w", r.Meta.Name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Team, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("team.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r)
}

// Upsert expects Meta.ID set by the caller.
func (s *Store) Upsert(ctx context.Context, t *Team) error {
	r, err := t.ToDirectory()
	if err != nil {
		return fmt.Errorf("team.Upsert: %w", err)
	}
	return meta.FromDirectoryError(s.rows.Upsert(ctx, r))
}

// Delete relies on the FK to cascade the team's projects.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
