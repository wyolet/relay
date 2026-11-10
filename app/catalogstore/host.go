// host.go is the data-access layer for Host. It maps Host domain structs to
// and from the sqlc-generated row types. Metadata JSONB encoding is delegated
// to pkg/meta; this file only knows about Host's Spec.

package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
)

// HostStore is the Host data-access type. Holds a sqlc Queries handle.
type HostStore struct {
	q *gen.Queries
}

// NewHostStore constructs a HostStore from an existing sqlc Queries handle.
// The caller owns the pgxpool / transaction; the store performs no
// connection mgmt.
func NewHostStore(q *gen.Queries) *HostStore { return &HostStore{q: q} }

// List returns every Host row in slug order. Cheap — catalog is small.
func (s *HostStore) List(ctx context.Context) ([]*host.Host, error) {
	rows, err := s.q.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("host.List: %w", err)
	}
	out := make([]*host.Host, 0, len(rows))
	for _, r := range rows {
		h, err := hostFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", r.Name, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// Upsert writes h. Caller is responsible for stamping Meta.ID.
func (s *HostStore) Upsert(ctx context.Context, h *host.Host) error {
	params, err := hostUpsertParams(h)
	if err != nil {
		return fmt.Errorf("host.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertHost(ctx, params))
}

// Get returns the Host with the given id, or (nil, nil) if not found.
func (s *HostStore) Get(ctx context.Context, id string) (*host.Host, error) {
	r, err := s.q.GetHost(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("host.Get: %w", err)
	}
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec host.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &host.Host{Meta: m, Spec: spec}, nil
}

// Delete removes a Host by id. sqlc returns nil on a no-rows delete;
// callers can pre-check existence via the snapshot if they need a 404.
func (s *HostStore) Delete(ctx context.Context, id string) error {
	return s.q.DeleteHost(ctx, id)
}

func hostFromRow(r gen.Host) (*host.Host, error) {
	m, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = r.CreatedAt.Time
	m.UpdatedAt = r.UpdatedAt.Time
	m.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec host.Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &host.Host{Meta: m, Spec: spec}, nil
}

func hostUpsertParams(h *host.Host) (gen.UpsertHostParams, error) {
	metaJSON, err := meta.MarshalJSONB(h.Meta)
	if err != nil {
		return gen.UpsertHostParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(h.Spec)
	if err != nil {
		return gen.UpsertHostParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := h.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertHostParams{}, err
	}
	return gen.UpsertHostParams{
		ID:              h.Meta.ID,
		Name:            h.Meta.Name,
		DisplayName:     h.Meta.DisplayName,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
