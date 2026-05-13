// store.go is the data-access layer for Provider. It maps Provider domain
// structs to and from the sqlc-generated row types, and is the only file in
// this package that talks SQL.
//
// The Store type is concrete — no interface declared here. Consumers (the
// snapshot composer, admin handlers, seed) each declare their own narrow
// interface locally if they want a test seam.
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

// ErrNotFound is returned when no Provider matches the requested id.
var ErrNotFound = errors.New("provider: not found")

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
		p, err := decode(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec)
		if err != nil {
			return nil, fmt.Errorf("provider.List decode %s: %w", r.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns the Provider with this id, or ErrNotFound. Implemented over
// List for now — the sqlc layer has no point-query for providers yet.
func (s *Store) Get(ctx context.Context, id string) (*Provider, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Meta.ID == id {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// Upsert writes p. p.Meta.ID must already be set; callers stamp ids before
// reaching the store (so the same id can flow through validation + seed
// remap + tx commit without surprises).
func (s *Store) Upsert(ctx context.Context, p *Provider) error {
	if p.Meta.ID == "" {
		return errors.New("provider.Upsert: Meta.ID required (stamp before write)")
	}
	metaJSON, err := encodeMetadata(p.Meta)
	if err != nil {
		return fmt.Errorf("provider.Upsert: encode metadata: %w", err)
	}
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return fmt.Errorf("provider.Upsert: encode spec: %w", err)
	}
	return s.q.UpsertProvider(ctx, gen.UpsertProviderParams{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	})
}

// Delete removes a Provider by id. Returns ErrNotFound on miss.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.q.DeleteProvider(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("provider.Delete: %w", err)
	}
	return nil
}

// ── encoding helpers ─────────────────────────────────────────────────────────

// metadataDoc is the on-disk JSONB shape of meta.Metadata. ID/Name/DisplayName
// live in their own columns so they're never duplicated here; description,
// owner, and labels are JSONB only.
type metadataDoc struct {
	Description string            `json:"description,omitempty"`
	Owner       ownerDoc          `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type ownerDoc struct {
	Kind meta.OwnerKind `json:"kind,omitempty"`
	ID   string         `json:"id,omitempty"`
}

func encodeMetadata(m meta.Metadata) ([]byte, error) {
	return json.Marshal(metadataDoc{
		Description: m.Description,
		Owner:       ownerDoc{Kind: m.Owner.Kind, ID: m.Owner.ID},
		Labels:      m.Labels,
	})
}

func decodeMetadata(raw []byte, m *meta.Metadata) error {
	if len(raw) == 0 {
		return nil
	}
	var d metadataDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	m.Description = d.Description
	m.Owner = meta.Owner{Kind: d.Kind(), ID: d.Owner.ID}
	m.Labels = d.Labels
	return nil
}

func (d metadataDoc) Kind() meta.OwnerKind { return d.Owner.Kind }

func decode(id, name, displayName string, metaJSON, specJSON []byte) (*Provider, error) {
	p := &Provider{
		Meta: meta.Metadata{ID: id, Name: name, DisplayName: displayName},
	}
	if err := decodeMetadata(metaJSON, &p.Meta); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if err := json.Unmarshal(specJSON, &p.Spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return p, nil
}
