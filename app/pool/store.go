// Spec.HostID lives in the host_id column and Spec.CredentialIDs in the pool_credentials junction, so deleting a host or a key reaches the pool through its FKs. Upsert writes both inside one transaction.

package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store holds a DB rather than Queries so Upsert can run a multi-table transaction.
type Store struct {
	db storage.DB
}

// NewStore constructs a Store bound to a pool or an open transaction.
func NewStore(db storage.DB) *Store { return &Store{db: db} }

// List returns every Pool row with its credentials in order.
func (s *Store) List(ctx context.Context) ([]*Pool, error) {
	q := gen.New(s.db)
	rows, err := q.ListPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("pool.List: %w", err)
	}
	keyRows, err := q.ListPoolCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("pool.List credentials: %w", err)
	}
	keysByPool := map[string][]string{}
	for _, r := range keyRows {
		keysByPool[r.PoolID] = append(keysByPool[r.PoolID], r.HostKeyID)
	}
	out := make([]*Pool, 0, len(rows))
	for _, r := range rows {
		p, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", r.Name, err)
		}
		p.Spec.CredentialIDs = keysByPool[p.Meta.ID]
		out = append(out, p)
	}
	return out, nil
}

// Get returns the Pool with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Pool, error) {
	q := gen.New(s.db)
	r, err := q.GetPool(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pool.Get: %w", err)
	}
	p, err := fromRow(r)
	if err != nil {
		return nil, err
	}
	keyRows, err := q.GetPoolCredentials(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("pool.Get credentials: %w", err)
	}
	for _, kr := range keyRows {
		p.Spec.CredentialIDs = append(p.Spec.CredentialIDs, kr.HostKeyID)
	}
	return p, nil
}

// Upsert writes p and replaces its credential list in one transaction.
func (s *Store) Upsert(ctx context.Context, p *Pool) error {
	params, err := toUpsertParams(p)
	if err != nil {
		return fmt.Errorf("pool.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pool.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	if err := meta.StaleIfNoRows(q.UpsertPool(ctx, params)); err != nil {
		return fmt.Errorf("pool.Upsert: pools: %w", err)
	}
	if err := q.DeletePoolCredentials(ctx, p.Meta.ID); err != nil {
		return fmt.Errorf("pool.Upsert: clear credentials: %w", err)
	}
	for i, id := range p.Spec.CredentialIDs {
		if err := q.InsertPoolCredential(ctx, gen.InsertPoolCredentialParams{
			PoolID:    p.Meta.ID,
			HostKeyID: id,
			Position:  int32(i),
		}); err != nil {
			return fmt.Errorf("pool.Upsert: insert credential %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pool.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a Pool by id; its credential rows and policy links cascade.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeletePool(ctx, id)
}

func fromRow(r gen.Pool) (*Pool, error) {
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
	spec.CredentialIDs = nil
	return &Pool{Meta: md, Spec: spec}, nil
}

func toUpsertParams(p *Pool) (gen.UpsertPoolParams, error) {
	metaJSON, err := meta.MarshalJSONB(p.Meta)
	if err != nil {
		return gen.UpsertPoolParams{}, fmt.Errorf("metadata: %w", err)
	}
	stored := p.Spec
	stored.HostID = ""
	stored.CredentialIDs = nil
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertPoolParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := p.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertPoolParams{}, err
	}
	return gen.UpsertPoolParams{
		ID:              p.Meta.ID,
		Name:            p.Meta.Name,
		DisplayName:     p.Meta.DisplayName,
		HostID:          p.Spec.HostID,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
