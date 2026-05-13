// store.go is the data-access layer for RateLimit. Mirrors the other entities.
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the RateLimit data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every RateLimit row.
func (s *Store) List(ctx context.Context) ([]*RateLimit, error) {
	rows, err := s.q.ListRateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("ratelimit.List: %w", err)
	}
	out := make([]*RateLimit, 0, len(rows))
	for _, r := range rows {
		rl, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("ratelimit %s: %w", r.Name, err)
		}
		out = append(out, rl)
	}
	return out, nil
}

// Upsert writes rl. Caller is responsible for stamping Meta.ID.
func (s *Store) Upsert(ctx context.Context, rl *RateLimit) error {
	params, err := toUpsertParams(rl)
	if err != nil {
		return fmt.Errorf("ratelimit.Upsert: %w", err)
	}
	return s.q.UpsertRateLimit(ctx, params)
}

// Get returns the RateLimit with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*RateLimit, error) {
	r, err := s.q.GetRateLimit(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ratelimit.Get: %w", err)
	}
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &RateLimit{Meta: m, Spec: spec}, nil
}

// Delete removes a RateLimit by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteRateLimit(ctx, id)
}

func fromRow(r gen.ListRateLimitsRow) (*RateLimit, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &RateLimit{Meta: md, Spec: spec}, nil
}

func toUpsertParams(rl *RateLimit) (gen.UpsertRateLimitParams, error) {
	metaJSON, err := meta.MarshalJSONB(rl.Meta)
	if err != nil {
		return gen.UpsertRateLimitParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(rl.Spec)
	if err != nil {
		return gen.UpsertRateLimitParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertRateLimitParams{
		ID:          rl.Meta.ID,
		Name:        rl.Meta.Name,
		DisplayName: rl.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
