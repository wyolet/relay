// Package secret is the Postgres-backed implementation of pkg/secret's
// storage seam: the secret_values table holding AES-GCM ciphertext for the
// "stored" backend. It satisfies pkg/secret.Store (Get/Put), .Rotator
// (transactional re-encryption), and .MaxVersioner, so the pure
// pkg/secret.StoredResolver can drive resolution, creation, and rotation
// without importing storage.
package secret

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

var (
	_ pkgsecret.Store        = (*Store)(nil)
	_ pkgsecret.Rotator      = (*Store)(nil)
	_ pkgsecret.MaxVersioner = (*Store)(nil)
)

// Store persists stored-secret ciphertext in the secret_values table. The
// pool is required for Rotate's transaction; pass nil only in tests that
// don't rotate.
type Store struct {
	q    *gen.Queries
	pool *pgxpool.Pool
}

// NewStore constructs the store over the shared query set + pool.
func NewStore(q *gen.Queries, pool *pgxpool.Pool) *Store {
	return &Store{q: q, pool: pool}
}

func (s *Store) Get(ctx context.Context, id string) (ciphertext, nonce []byte, keyVersion int32, err error) {
	row, err := s.q.GetSecretValue(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, 0, fmt.Errorf("secret %q not found", id)
		}
		return nil, nil, 0, fmt.Errorf("secret.Store.Get %q: %w", id, err)
	}
	return row.Ciphertext, row.Nonce, row.KeyVersion, nil
}

func (s *Store) Put(ctx context.Context, id string, ciphertext, nonce []byte, keyVersion int32) error {
	return s.q.UpsertSecretValue(ctx, gen.UpsertSecretValueParams{
		ID:         id,
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: keyVersion,
	})
}

// Delete removes a stored secret. Called when a HostKey switches away from
// stored mode or is deleted.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteSecretValue(ctx, id)
}

func (s *Store) MaxKeyVersion(ctx context.Context) (int32, error) {
	return s.q.MaxSecretValueKeyVersion(ctx)
}

// Rotate re-encrypts every secret_values row within one transaction,
// stamping newKeyVersion. reencrypt (supplied by pkg/secret, holding the
// key material) maps old ciphertext → new. All-or-nothing: a failure rolls
// back, leaving every row on the old key.
func (s *Store) Rotate(ctx context.Context, newKeyVersion int32,
	reencrypt func(ct, nonce []byte) (newCt, newNonce []byte, err error)) (int, error) {
	if s.pool == nil {
		return 0, errors.New("secret.Store.Rotate: pool not configured")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("secret.Store.Rotate begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)
	rows, err := qtx.ListSecretValuesForRotation(ctx)
	if err != nil {
		return 0, fmt.Errorf("secret.Store.Rotate list: %w", err)
	}
	for _, r := range rows {
		nct, nnonce, err := reencrypt(r.Ciphertext, r.Nonce)
		if err != nil {
			return 0, fmt.Errorf("secret.Store.Rotate reencrypt %s: %w", r.ID, err)
		}
		if err := qtx.UpsertSecretValue(ctx, gen.UpsertSecretValueParams{
			ID:         r.ID,
			Ciphertext: nct,
			Nonce:      nnonce,
			KeyVersion: newKeyVersion,
		}); err != nil {
			return 0, fmt.Errorf("secret.Store.Rotate update %s: %w", r.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("secret.Store.Rotate commit: %w", err)
	}
	return len(rows), nil
}
