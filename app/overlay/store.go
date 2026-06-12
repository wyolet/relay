// store.go is the data-access layer for Overlay. Mirrors app/model/store.go.
package overlay

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Overlay data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every overlay row.
func (s *Store) List(ctx context.Context) ([]*Overlay, error) {
	rows, err := s.q.ListOverlays(ctx)
	if err != nil {
		return nil, fmt.Errorf("overlay.List: %w", err)
	}
	out := make([]*Overlay, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Get returns the overlay targeting (kind, resourceID), or nil when none
// exists.
func (s *Store) Get(ctx context.Context, kind, resourceID string) (*Overlay, error) {
	r, err := s.q.GetOverlay(ctx, gen.GetOverlayParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("overlay.Get: %w", err)
	}
	return fromRow(r), nil
}

// Upsert writes o. The PG trigger emits the catalog NOTIFY.
func (s *Store) Upsert(ctx context.Context, o *Overlay) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if err := s.q.UpsertOverlay(ctx, gen.UpsertOverlayParams{
		Kind:       o.Kind,
		ResourceID: o.ResourceID,
		Patch:      o.Patch,
	}); err != nil {
		return fmt.Errorf("overlay.Upsert: %w", err)
	}
	return nil
}

// Delete removes the overlay targeting (kind, resourceID). Idempotent.
func (s *Store) Delete(ctx context.Context, kind, resourceID string) error {
	if err := s.q.DeleteOverlay(ctx, gen.DeleteOverlayParams{Kind: kind, ResourceID: resourceID}); err != nil {
		return fmt.Errorf("overlay.Delete: %w", err)
	}
	return nil
}

func fromRow(r gen.Overlay) *Overlay {
	return &Overlay{
		Kind:       r.Kind,
		ResourceID: r.ResourceID,
		Patch:      r.Patch,
		UpdatedAt:  r.UpdatedAt.Time,
	}
}
