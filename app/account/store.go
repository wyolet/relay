// Spec.HostID lives in the host_id column so a host delete cascades; the spec JSONB carries the rest.

package account

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

// Store is the Account data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Account row.
func (s *Store) List(ctx context.Context) ([]*Account, error) {
	rows, err := s.q.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("account.List: %w", err)
	}
	out := make([]*Account, 0, len(rows))
	for _, r := range rows {
		a, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", r.Name, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Get returns the Account with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Account, error) {
	r, err := s.q.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("account.Get: %w", err)
	}
	return fromRow(r)
}

// Upsert writes a. The caller sets Meta.ID.
func (s *Store) Upsert(ctx context.Context, a *Account) error {
	params, err := toUpsertParams(a)
	if err != nil {
		return fmt.Errorf("account.Upsert: %w", err)
	}
	return meta.StaleIfNoRows(s.q.UpsertAccount(ctx, params))
}

// Delete removes an Account by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteAccount(ctx, id)
}

func fromRow(r gen.Account) (*Account, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	spec.HostID = r.HostID
	return &Account{Meta: md, Spec: spec}, nil
}

func toUpsertParams(a *Account) (gen.UpsertAccountParams, error) {
	metaJSON, err := meta.MarshalJSONB(a.Meta)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("metadata: %w", err)
	}
	stored := a.Spec
	stored.HostID = ""
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := a.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertAccountParams{}, err
	}
	return gen.UpsertAccountParams{
		ID:              a.Meta.ID,
		Name:            a.Meta.Name,
		DisplayName:     a.Meta.DisplayName,
		HostID:          a.Spec.HostID,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
