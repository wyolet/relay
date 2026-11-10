// provider.go is the data-access layer for Provider. It maps Provider domain
// structs to and from the sqlc-generated row types. Metadata JSONB encoding
// is delegated to pkg/meta; this file only knows about Provider's Spec.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// ProviderStore is the Provider data-access type. Holds a sqlc Queries handle.
type ProviderStore struct {
	q *gen.Queries
}

// NewProviderStore constructs a ProviderStore from an existing sqlc Queries
// handle. The caller owns the pgxpool / transaction; the store performs no
// connection mgmt.
func NewProviderStore(q *gen.Queries) *ProviderStore { return &ProviderStore{q: q} }

// List returns every Provider row in slug order. Cheap — catalog is small.
func (s *ProviderStore) List(ctx context.Context) ([]*provider.Provider, error) {
	rows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider.List: %w", err)
	}
	out := make([]*provider.Provider, 0, len(rows))
	for _, r := range rows {
		p, err := providerFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Upsert writes p. Caller is responsible for stamping Meta.ID.
func (s *ProviderStore) Upsert(ctx context.Context, p *provider.Provider) error {
	params, err := providerUpsertParams(p)
	if err != nil {
		return fmt.Errorf("provider.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertProvider(ctx, params))
}

// Get returns the Provider with the given id, or (nil, nil) if not found.
func (s *ProviderStore) Get(ctx context.Context, id string) (*provider.Provider, error) {
	r, err := s.q.GetProvider(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("provider.Get: %w", err)
	}
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec provider.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &provider.Provider{Meta: m, Spec: spec}, nil
}

// Delete removes a Provider by id. sqlc returns nil on a no-rows delete;
// callers can pre-check existence via the snapshot if they need a 404.
func (s *ProviderStore) Delete(ctx context.Context, id string) error {
	return s.q.DeleteProvider(ctx, id)
}

func providerFromRow(r gen.ListProvidersRow) (*provider.Provider, error) {
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec provider.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &provider.Provider{Meta: m, Spec: spec}, nil
}

func providerUpsertParams(p *provider.Provider) (gen.UpsertProviderParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertProviderParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return gen.UpsertProviderParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := p.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertProviderParams{}, err
	}
	return gen.UpsertProviderParams{
		ID:              p.Meta.ID,
		Name:            p.Meta.Name,
		DisplayName:     p.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
