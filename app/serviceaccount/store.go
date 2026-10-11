// Service accounts are stored by the auth module's directory store; project_id is a real column there (FK cascade), so deleting a Project drops its accounts without parsing JSONB.

package serviceaccount

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
)

type Store struct {
	rows directory.Store[directory.ServiceAccount]
}

func NewStore(db storage.DB) *Store { return &Store{rows: authpg.New(db).ServiceAccounts} }

// Directory is the auth module's store under this one, for writes that run through the directory.
func (s *Store) Directory() directory.Store[directory.ServiceAccount] { return s.rows }

func (s *Store) List(ctx context.Context) ([]*ServiceAccount, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("serviceaccount.List: %w", err)
	}
	out := make([]*ServiceAccount, 0, len(rows))
	for _, r := range rows {
		sa, err := FromDirectory(r)
		if err != nil {
			return nil, fmt.Errorf("serviceaccount %s: %w", r.Meta.Name, err)
		}
		out = append(out, sa)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*ServiceAccount, error) {
	r, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("serviceaccount.Get: %w", err)
	}
	if r == nil {
		return nil, nil
	}
	return FromDirectory(r)
}

// Upsert expects Meta.ID set by the caller and re-derives Owner from ProjectID.
func (s *Store) Upsert(ctx context.Context, sa *ServiceAccount) error {
	sa.StampOwner()
	r, err := sa.ToDirectory()
	if err != nil {
		return fmt.Errorf("serviceaccount.Upsert: %w", err)
	}
	return meta.FromDirectoryError(s.rows.Upsert(ctx, r))
}

// Delete relies on the FK to cascade the account's keys.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}
