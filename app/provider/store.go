// store.go is the data-access layer for Provider. It maps Provider domain
// structs to and from the sqlc-generated row types. Metadata JSONB encoding
// is delegated to app/meta; this file only knows about Provider's Spec.
//
// Store is concrete — no interface declared here. Consumers (snapshot
// composer, admin handlers, seed) each declare their own narrow interface
// locally if they want a test seam.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Provider data-access type. Holds a sqlc Queries handle.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle. The
// caller owns the pgxpool / transaction; Store performs no connection mgmt.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Provider row in slug order. Cheap — catalog is small.
func (s *Store) List(ctx context.Context) ([]*Provider, error) {
	rows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider.List: %w", err)
	}
	out := make([]*Provider, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Upsert writes p. Caller is responsible for stamping Meta.ID.
func (s *Store) Upsert(ctx context.Context, p *Provider) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("provider.Upsert: %w", err)
	}
	return s.q.UpsertProvider(ctx, params)
}

// Get returns the Provider with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Provider, error) {
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
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Provider{Meta: m, Spec: spec}, nil
}

// Delete removes a Provider by id. sqlc returns nil on a no-rows delete;
// callers can pre-check existence via the snapshot if they need a 404.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteProvider(ctx, id)
}

func fromRow(r gen.ListProvidersRow) (*Provider, error) {
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Provider{Meta: m, Spec: spec}, nil
}

func toUpsertParams(p *Provider) (gen.UpsertProviderParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertProviderParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return gen.UpsertProviderParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertProviderParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
