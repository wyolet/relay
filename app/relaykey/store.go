// store.go is the data-access layer for RelayKey. Same shape as the other
// entity stores; sha256(plaintext) is the caller's responsibility — the
// plaintext never enters this package.
package relaykey

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the RelayKey data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every RelayKey row.
func (s *Store) List(ctx context.Context) ([]*RelayKey, error) {
	rows, err := s.q.ListRelayKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("relaykey.List: %w", err)
	}
	out := make([]*RelayKey, 0, len(rows))
	for _, r := range rows {
		k, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("relaykey %s: %w", r.Name, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Upsert writes k. Caller is responsible for stamping Meta.ID and for
// computing Spec.KeyHash from the plaintext.
func (s *Store) Upsert(ctx context.Context, k *RelayKey) error {
	params, err := toUpsertParams(k)
	if err != nil {
		return fmt.Errorf("relaykey.Upsert: %w", err)
	}
	return s.q.UpsertRelayKey(ctx, params)
}

// Delete removes a RelayKey by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteRelayKey(ctx, id)
}

func fromRow(r gen.ListRelayKeysRow) (*RelayKey, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return &RelayKey{Meta: md, Spec: spec}, nil
}

func toUpsertParams(k *RelayKey) (gen.UpsertRelayKeyParams, error) {
	metaJSON, err := meta.MarshalJSONB(k.Meta)
	if err != nil {
		return gen.UpsertRelayKeyParams{}, fmt.Errorf("metadata: %w", err)
	}
	specJSON, err := json.Marshal(k.Spec)
	if err != nil {
		return gen.UpsertRelayKeyParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertRelayKeyParams{
		ID:          k.Meta.ID,
		Name:        k.Meta.Name,
		DisplayName: k.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
