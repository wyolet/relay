// Role bindings are stored by the auth module's directory store. Subjects live in the role_binding_subjects junction, where a subject with an id also fills the matching FK column so deleting a user or service account drops it from every binding.

package rolebinding

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.RoleBinding]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).RoleBindings} }

func (s *Store) List(ctx context.Context) ([]*RoleBinding, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("rolebinding.List: %w", err)
	}
	out := make([]*RoleBinding, 0, len(rows))
	for _, r := range rows {
		out = append(out, FromDirectory(r))
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*RoleBinding, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("rolebinding.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r), nil
}

// Upsert re-derives Owner from Spec.Scope.
func (s *Store) Upsert(ctx context.Context, b *RoleBinding) error {
	b.StampOwner()
	if err := s.rows.Upsert(ctx, b.ToDirectory()); err != nil {
		return fmt.Errorf("rolebinding.Upsert: %w", meta.FromDirectoryError(err))
	}
	return nil
}

// Delete relies on the FK to cascade the junction rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
