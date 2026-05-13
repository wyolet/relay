// store.go is the data-access layer for Host. It maps Host domain structs to
// and from the sqlc-generated row types. Metadata JSONB encoding is delegated
// to app/meta; this file only knows about Host's Spec.
//
// Store is concrete — no interface declared here. Consumers (snapshot
// composer, admin handlers, seed) each declare their own narrow interface
// locally if they want a test seam.
package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Host data-access type. Holds a sqlc Queries handle.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle. The
// caller owns the pgxpool / transaction; Store performs no connection mgmt.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Host row in slug order. Cheap — catalog is small.
func (s *Store) List(ctx context.Context) ([]*Host, error) {
	rows, err := s.q.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("host.List: %w", err)
	}
	out := make([]*Host, 0, len(rows))
	for _, r := range rows {
		h, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", r.Name, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// Upsert writes h. Caller is responsible for stamping Meta.ID.
func (s *Store) Upsert(ctx context.Context, h *Host) error {
	params, err := toUpsertParams(h)
	if err != nil {
		return fmt.Errorf("host.Upsert: %w", err)
	}
	return s.q.UpsertHost(ctx, params)
}

// Delete removes a Host by id. sqlc returns nil on a no-rows delete;
// callers can pre-check existence via the snapshot if they need a 404.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteHost(ctx, id)
}

func fromRow(r gen.ListHostsRow) (*Host, error) {
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &Host{Meta: m, Spec: spec}, nil
}

func toUpsertParams(h *Host) (gen.UpsertHostParams, error) {
	metaJSON, err := meta.MarshalJSONB(h.Meta)
	if err != nil {
		return gen.UpsertHostParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(h.Spec)
	if err != nil {
		return gen.UpsertHostParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertHostParams{
		ID:          h.Meta.ID,
		Name:        h.Meta.Name,
		DisplayName: h.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
