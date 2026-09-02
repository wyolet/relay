// store.go is the data-access layer for Key. Same shape as the other
// entity stores; sha256(plaintext) is the caller's responsibility — the
// plaintext never enters this package.
//
// The principal and the rotation hashes live in real columns (FK cascade,
// unique index) as well as the spec JSONB; the columns win on read.
package key

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the Key data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Key row.
func (s *Store) List(ctx context.Context) ([]*Key, error) {
	rows, err := s.q.ListRelayKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("key.List: %w", err)
	}
	out := make([]*Key, 0, len(rows))
	for _, r := range rows {
		k, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", r.Name, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Upsert writes k. Caller is responsible for stamping Meta.ID and for
// computing Spec.KeyHash from the plaintext.
func (s *Store) Upsert(ctx context.Context, k *Key) error {
	params, err := toUpsertParams(k)
	if err != nil {
		return fmt.Errorf("key.Upsert: %w", err)
	}
	return s.q.UpsertRelayKey(ctx, params)
}

// Get returns the Key with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Key, error) {
	r, err := s.q.GetRelayKey(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("key.Get: %w", err)
	}
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
	applyColumns(&spec, r.PreviousKeyHash, r.PrincipalSaID, r.PrincipalUserID)
	return &Key{Meta: md, Spec: spec}, nil
}

// Delete removes a Key by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteRelayKey(ctx, id)
}

func fromRow(r gen.ListRelayKeysRow) (*Key, error) {
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
	applyColumns(&spec, r.PreviousKeyHash, r.PrincipalSaID, r.PrincipalUserID)
	return &Key{Meta: md, Spec: spec}, nil
}

// applyColumns overwrites the spec's principal + previous hash from the
// relational columns, which the migration backfilled and every write keeps
// in step.
func applyColumns(spec *Spec, prevHash, saID, userID pgtype.Text) {
	spec.PreviousKeyHash = prevHash.String
	switch {
	case saID.Valid:
		spec.Principal = Principal{Kind: PrincipalServiceAccount, ID: saID.String}
	case userID.Valid:
		spec.Principal = Principal{Kind: PrincipalUser, ID: userID.String}
	}
}

func toUpsertParams(k *Key) (gen.UpsertRelayKeyParams, error) {
	metaJSON, err := meta.MarshalJSONB(k.Meta)
	if err != nil {
		return gen.UpsertRelayKeyParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Relational fields live in columns; keep them out of the spec JSONB so
	// there is one source of truth per field (applyColumns reads them back).
	stored := k.Spec
	stored.Principal = Principal{}
	stored.PreviousKeyHash = ""
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertRelayKeyParams{}, fmt.Errorf("spec: %w", err)
	}
	params := gen.UpsertRelayKeyParams{
		ID:          k.Meta.ID,
		Name:        k.Meta.Name,
		DisplayName: k.Meta.DisplayName,
		KeyHash:     k.Spec.KeyHash,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}
	if k.Spec.PreviousKeyHash != "" {
		params.PreviousKeyHash = pgtype.Text{String: k.Spec.PreviousKeyHash, Valid: true}
	}
	switch k.Spec.Principal.Kind {
	case PrincipalServiceAccount:
		params.PrincipalSaID = pgtype.Text{String: k.Spec.Principal.ID, Valid: true}
	case PrincipalUser:
		params.PrincipalUserID = pgtype.Text{String: k.Spec.Principal.ID, Valid: true}
	}
	return params, nil
}
