// Groups are stored by the auth module's directory store, which keeps Spec.MemberIDs in the group_members junction so the FK to users drops a deleted user's memberships.

package group

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.Group]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).Groups} }

// Directory is the auth module's store under this one, for writes that run through the directory.
func (s *Store) Directory() directory.Store[directory.Group] { return s.rows }

func (s *Store) List(ctx context.Context) ([]*Group, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("group.List: %w", err)
	}
	out := make([]*Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, FromDirectory(r))
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Group, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("group.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r), nil
}

func (s *Store) Upsert(ctx context.Context, g *Group) error {
	if err := s.rows.Upsert(ctx, g.ToDirectory()); err != nil {
		return fmt.Errorf("group.Upsert: %w", meta.FromDirectoryError(err))
	}
	return nil
}

// Delete relies on the FK to cascade the junction rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
