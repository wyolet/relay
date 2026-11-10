// binding.go is the data-access layer for HostBinding. model_id / host_id /
// pricing_id are real columns (FK integrity + builder joins without JSONB
// parsing), so they are the source of truth for those refs — stripped from
// the JSONB spec on write and rebuilt from the columns on read, mirroring
// how pricing treats host_id. The rest of the spec (adapter, upstreamName,
// enabled, the snapshots subset) lives in the JSONB.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/meta"
)

// BindingStore is the HostBinding data-access type.
type BindingStore struct {
	db gen.DBTX
}

// NewBindingStore constructs a BindingStore bound to a pool or an open
// transaction.
func NewBindingStore(db gen.DBTX) *BindingStore { return &BindingStore{db: db} }

// List returns every HostBinding row.
func (s *BindingStore) List(ctx context.Context) ([]*binding.Binding, error) {
	rows, err := gen.New(s.db).ListHostBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("binding.List: %w", err)
	}
	out := make([]*binding.Binding, 0, len(rows))
	for _, r := range rows {
		b, err := bindingFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("binding %s: %w", r.Name, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Get returns the HostBinding with the given id, or (nil, nil) if not found.
func (s *BindingStore) Get(ctx context.Context, id string) (*binding.Binding, error) {
	r, err := gen.New(s.db).GetHostBinding(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("binding.Get: %w", err)
	}
	return bindingFromRow(gen.HostBinding(r))
}

// Upsert writes b. Caller stamps Meta.ID.
func (s *BindingStore) Upsert(ctx context.Context, b *binding.Binding) error {
	params, err := bindingUpsertParams(b)
	if err != nil {
		return fmt.Errorf("binding.Upsert: %w", err)
	}
	if err := meta.StaleIfNoRows(gen.New(s.db).UpsertHostBinding(ctx, params)); err != nil {
		return fmt.Errorf("binding.Upsert: %w", err)
	}
	return nil
}

// Delete removes a HostBinding by id.
func (s *BindingStore) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeleteHostBinding(ctx, id)
}

func bindingFromRow(r gen.HostBinding) (*binding.Binding, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec binding.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	// Columns are the source of truth for the FK refs.
	spec.ModelID = r.ModelID
	spec.HostID = r.HostID
	spec.PricingID = r.PricingID.String // "" when NULL (.Valid false → zero value)
	return &binding.Binding{Meta: md, Spec: spec}, nil
}

func bindingUpsertParams(b *binding.Binding) (gen.UpsertHostBindingParams, error) {
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
	ver, ok, err := b.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertHostBindingParams{}, err
	}
	return gen.UpsertHostBindingParams{
		ID:              b.Meta.ID,
		Name:            b.Meta.Name,
		DisplayName:     b.Meta.DisplayName,
		ModelID:         b.Spec.ModelID,
		HostID:          b.Spec.HostID,
		PricingID:       pricing,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
