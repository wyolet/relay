// ratelimit.go is the data-access layer for RateLimit. Mirrors the other
// catalog kinds.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// RateLimitStore is the RateLimit data-access type.
type RateLimitStore struct {
	q *gen.Queries
}

// NewRateLimitStore constructs a RateLimitStore from an existing sqlc Queries
// handle.
func NewRateLimitStore(q *gen.Queries) *RateLimitStore { return &RateLimitStore{q: q} }

// List returns every RateLimit row.
func (s *RateLimitStore) List(ctx context.Context) ([]*ratelimit.RateLimit, error) {
	rows, err := s.q.ListRateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("ratelimit.List: %w", err)
	}
	out := make([]*ratelimit.RateLimit, 0, len(rows))
	for _, r := range rows {
		rl, err := rateLimitFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("ratelimit %s: %w", r.Name, err)
		}
		out = append(out, rl)
	}
	return out, nil
}

// Upsert writes rl. Caller is responsible for stamping Meta.ID.
func (s *RateLimitStore) Upsert(ctx context.Context, rl *ratelimit.RateLimit) error {
	params, err := rateLimitUpsertParams(rl)
	if err != nil {
		return fmt.Errorf("ratelimit.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertRateLimit(ctx, params))
}

// Get returns the RateLimit with the given id, or (nil, nil) if not found.
func (s *RateLimitStore) Get(ctx context.Context, id string) (*ratelimit.RateLimit, error) {
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
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec ratelimit.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &ratelimit.RateLimit{Meta: m, Spec: spec}, nil
}

// Delete removes a RateLimit by id.
func (s *RateLimitStore) Delete(ctx context.Context, id string) error {
	return s.q.DeleteRateLimit(ctx, id)
}

func rateLimitFromRow(r gen.ListRateLimitsRow) (*ratelimit.RateLimit, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec ratelimit.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &ratelimit.RateLimit{Meta: md, Spec: spec}, nil
}

func rateLimitUpsertParams(rl *ratelimit.RateLimit) (gen.UpsertRateLimitParams, error) {
	metaJSON, err := meta.MarshalJSONB(rl.Meta)
	if err != nil {
		return gen.UpsertRateLimitParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(rl.Spec)
	if err != nil {
		return gen.UpsertRateLimitParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := rl.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertRateLimitParams{}, err
	}
	return gen.UpsertRateLimitParams{
		ID:              rl.Meta.ID,
		Name:            rl.Meta.Name,
		DisplayName:     rl.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
