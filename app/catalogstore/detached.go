package catalogstore

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/internal/storage/gen"
)

// DetachedStore answers whether any catalog row is detached, in one query
// cheap enough to ask on every catalog poll.
type DetachedStore struct {
	q *gen.Queries
}

// NewDetachedStore constructs a DetachedStore from an existing sqlc Queries handle.
func NewDetachedStore(q *gen.Queries) *DetachedStore { return &DetachedStore{q: q} }

// Any reports whether a row of a catalog kind carries CatalogDetachedAt.
func (s *DetachedStore) Any(ctx context.Context) (bool, error) {
	v, err := s.q.AnyCatalogDetached(ctx)
	if err != nil {
		return false, fmt.Errorf("detached.Any: %w", err)
	}
	return v.Valid && v.Bool, nil
}
