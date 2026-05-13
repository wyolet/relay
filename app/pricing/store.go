// store.go is the data-access layer for Pricing. TargetModelIDs live in the
// pricing_models junction (not JSONB), so Upsert fans out across pricings +
// pricing_models inside a single transaction. host_id is a real column,
// hydrated alongside the rest of the row on List.
package pricing

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Pricing data-access type. Holds a pool so Upsert can run a
// multi-table transaction.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store bound to a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns every Pricing row with Spec.TargetModelIDs hydrated from the
// pricing_models junction in declaration order.
func (s *Store) List(ctx context.Context) ([]*Pricing, error) {
	q := gen.New(s.pool)
	rows, err := q.ListPricings(ctx)
	if err != nil {
		return nil, fmt.Errorf("pricing.List: %w", err)
	}
	modelRows, err := q.ListPricingModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("pricing.List models: %w", err)
	}
	modelsByPricing := map[string][]string{}
	for _, r := range modelRows {
		modelsByPricing[r.PricingID] = append(modelsByPricing[r.PricingID], r.ModelID)
	}
	out := make([]*Pricing, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("pricing %s: %w", r.Name, err)
		}
		p.Spec.TargetModelIDs = modelsByPricing[p.Meta.ID]
		out = append(out, p)
	}
	return out, nil
}

// Upsert writes p across pricings + pricing_models in a single tx.
// Caller stamps Meta.ID and Owner.ID (host id).
func (s *Store) Upsert(ctx context.Context, p *Pricing) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("pricing.Upsert: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pricing.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := q.UpsertPricing(ctx, params); err != nil {
		return fmt.Errorf("pricing.Upsert: pricings: %w", err)
	}
	if err := q.DeletePricingModels(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("pricing.Upsert: clear models: %w", err)
	}
	for i, id := range p.Spec.TargetModelIDs {
		if err := q.InsertPricingModel(ctx, gen.InsertPricingModelParams{
			PricingID: p.Meta.ID,
			ModelID:   id,
			Position:  int32(i),
		}); err != nil {
			return fmt.Errorf("pricing.Upsert: insert model %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pricing.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a Pricing by id. Junction rows cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.pool).DeletePricing(ctx, id)
}

func fromRow(r gen.ListPricingsRow) (*Pricing, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	// host_id is a column — overrides anything in JSONB.
	md.Owner = meta.Owner{Kind: meta.OwnerHost, ID: r.HostID}
	return &Pricing{Meta: md, Spec: spec}, nil
}

func toUpsertParams(p *Pricing) (gen.UpsertPricingParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPricingParams{}, fmt.Errorf("metadata: %w", err)
	}
	// TargetModelIDs lives in the junction; strip from JSONB.
	specCopy := p.Spec
	specCopy.TargetModelIDs = nil
	specJSON, err := json.Marshal(specCopy)
	if err != nil {
		return gen.UpsertPricingParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertPricingParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		HostID:      p.Meta.Owner.ID,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
