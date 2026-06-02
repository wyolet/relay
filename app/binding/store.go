// store.go is the data-access layer for HostBinding. model_id / host_id /
// pricing_id are real columns (FK integrity + builder joins without JSONB
// parsing), so they are the source of truth for those refs — stripped from
// the JSONB spec on write and rebuilt from the columns on read, mirroring
// how pricing treats host_id. The rest of the spec (adapter, upstreamName,
// enabled, the snapshots subset) lives in the JSONB.
package binding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the HostBinding data-access type.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store bound to a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns every HostBinding row.
func (s *Store) List(ctx context.Context) ([]*Binding, error) {
	rows, err := gen.New(s.pool).ListHostBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("binding.List: %w", err)
	}
	out := make([]*Binding, 0, len(rows))
	for _, r := range rows {
		b, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("binding %s: %w", r.Name, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Get returns the HostBinding with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Binding, error) {
	r, err := gen.New(s.pool).GetHostBinding(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("binding.Get: %w", err)
	}
	return fromRow(gen.HostBinding(r))
}

// Upsert writes b. Caller stamps Meta.ID.
func (s *Store) Upsert(ctx context.Context, b *Binding) error {
	params, err := toUpsertParams(b)
	if err != nil {
		return fmt.Errorf("binding.Upsert: %w", err)
	}
	if err := gen.New(s.pool).UpsertHostBinding(ctx, params); err != nil {
		return fmt.Errorf("binding.Upsert: %w", err)
	}
	return nil
}

// Delete removes a HostBinding by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.pool).DeleteHostBinding(ctx, id)
}

func fromRow(r gen.HostBinding) (*Binding, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	// Columns are the source of truth for the FK refs.
	spec.ModelID = r.ModelID
	spec.HostID = r.HostID
	spec.PricingID = r.PricingID.String // "" when NULL (.Valid false → zero value)
	return &Binding{Meta: md, Spec: spec}, nil
}

func toUpsertParams(b *Binding) (gen.UpsertHostBindingParams, error) {
	metaJSON, err := meta.MarshalJSONB(b.Meta)
	if err != nil {
		return gen.UpsertHostBindingParams{}, fmt.Errorf("metadata: %w", err)
	}
	// FK refs live in columns; strip from JSONB to avoid drift.
	specCopy := b.Spec
	specCopy.ModelID = ""
	specCopy.HostID = ""
	specCopy.PricingID = ""
	specJSON, err := json.Marshal(specCopy)
	if err != nil {
		return gen.UpsertHostBindingParams{}, fmt.Errorf("spec: %w", err)
	}
	var pricing pgtype.Text
	if b.Spec.PricingID != "" {
		pricing = pgtype.Text{String: b.Spec.PricingID, Valid: true}
	}
	return gen.UpsertHostBindingParams{
		ID:          b.Meta.ID,
		Name:        b.Meta.Name,
		DisplayName: b.Meta.DisplayName,
		ModelID:     b.Spec.ModelID,
		HostID:      b.Spec.HostID,
		PricingID:   pricing,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
