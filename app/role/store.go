// Roles are stored by the auth module's directory store; the rules live in the spec JSONB and a Role has no relational columns of its own.

package role

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.Role]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).Roles} }

func (s *Store) List(ctx context.Context) ([]*Role, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("role.List: %w", err)
	}
	out := make([]*Role, 0, len(rows))
	for _, r := range rows {
		out = append(out, FromDirectory(r))
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Role, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("role.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r), nil
}

// Upsert expects Meta.ID set by the caller.
func (s *Store) Upsert(ctx context.Context, r *Role) error {
	return meta.FromDirectoryError(s.rows.Upsert(ctx, r.ToDirectory()))
}

// Delete relies on the FK to cascade the role's bindings.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
