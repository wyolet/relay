package stored

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/secret/keyring"
)

var _ keyring.Store = (*KeyStore)(nil)

// KeyStore keeps the key ring's data keys in secret_data_keys and the target KEK in secret_keyring. Writes to secret_values are checked against it by that table's trigger, so a process that missed a rotation or a destroy cannot write under a key that is no longer its scope's Primary.
type KeyStore struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

// NewKeyStore builds a KeyStore over pool.
func NewKeyStore(pool *pgxpool.Pool) *KeyStore {
	return &KeyStore{pool: pool, q: gen.New(pool)}
}

func (s *KeyStore) List(ctx context.Context) ([]keyring.DataKey, error) {
	rows, err := s.q.ListSecretDataKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("secret: list data keys: %w", err)
	}
	out := make([]keyring.DataKey, len(rows))
	for i, r := range rows {
		out[i] = dataKey(r.ID, r.Scope, r.State, r.KekID, r.Wrapped, r.CreatedAt, r.DemotedAt)
	}
	return out, nil
}

func (s *KeyStore) Get(ctx context.Context, id [16]byte) (keyring.DataKey, error) {
	r, err := s.q.GetSecretDataKey(ctx, uuid(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return keyring.DataKey{}, keyring.ErrUnknownKey
	}
	if err != nil {
		return keyring.DataKey{}, fmt.Errorf("secret: get data key: %w", err)
	}
	return dataKey(r.ID, r.Scope, r.State, r.KekID, r.Wrapped, r.CreatedAt, r.DemotedAt), nil
}

func (s *KeyStore) Primary(ctx context.Context, scope string) (keyring.DataKey, error) {
	r, err := s.q.GetPrimarySecretDataKey(ctx, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return keyring.DataKey{}, keyring.ErrUnknownKey
	}
	if err != nil {
		return keyring.DataKey{}, fmt.Errorf("secret: get primary data key: %w", err)
	}
	return dataKey(r.ID, r.Scope, r.State, r.KekID, r.Wrapped, r.CreatedAt, r.DemotedAt), nil
}

func (s *KeyStore) TargetKEK(ctx context.Context) ([16]byte, error) {
	var out [16]byte
	b, err := s.q.GetSecretKeyringTarget(ctx)
	if err != nil {
		return out, fmt.Errorf("secret: read target KEK: %w", err)
	}
	if len(b) != 0 && len(b) != len(out) {
		return out, fmt.Errorf("secret: target KEK id has %d bytes", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// SetTargetKEK refuses a zero id: unset is only the initial state.
func (s *KeyStore) SetTargetKEK(ctx context.Context, id, expected [16]byte) error {
	if id == ([16]byte{}) {
		return errors.New("secret: zero target KEK id")
	}
	var exp []byte
	if expected != ([16]byte{}) {
		exp = expected[:]
	}
	n, err := s.q.SetSecretKeyringTarget(ctx, gen.SetSecretKeyringTargetParams{Target: id[:], Expected: exp})
	if err != nil {
		return fmt.Errorf("secret: set target KEK: %w", err)
	}
	if n == 0 {
		return keyring.ErrConflict
	}
	return nil
}

// InsertPrimary demotes the previous Primary with demoted_at taken from the database clock.
func (s *KeyStore) InsertPrimary(ctx context.Context, k keyring.DataKey, expectedPrimary *[16]byte) error {
	if k.State != keyring.Primary || k.Wrapped == nil {
		return errors.New("secret: InsertPrimary needs a Primary key with wrapped bytes")
	}
	return s.inTx(ctx, func(q *gen.Queries) error {
		cur, err := q.LockPrimarySecretDataKey(ctx, k.Scope)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if expectedPrimary != nil {
				return keyring.ErrConflict
			}
		case err != nil:
			return fmt.Errorf("secret: lock primary data key: %w", err)
		default:
			if expectedPrimary == nil || cur.Bytes != *expectedPrimary {
				return keyring.ErrConflict
			}
			if err := q.DemoteSecretDataKey(ctx, cur); err != nil {
				return fmt.Errorf("secret: demote data key: %w", err)
			}
		}
		err = q.InsertSecretDataKey(ctx, gen.InsertSecretDataKeyParams{ID: uuid(k.ID), Scope: k.Scope, KekID: k.KEKID[:], Wrapped: k.Wrapped})
		// Two first keys for one scope: the one-primary index refuses the second.
		if isUniqueViolation(err) {
			return keyring.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("secret: insert data key: %w", err)
		}
		return nil
	})
}

func (s *KeyStore) UpdateWrapped(ctx context.Context, id, fromKEK, toKEK [16]byte, wrapped []byte) error {
	n, err := s.q.UpdateSecretDataKeyWrapped(ctx, gen.UpdateSecretDataKeyWrappedParams{
		ID: uuid(id), Wrapped: wrapped, ToKek: toKEK[:], FromKek: fromKEK[:],
	})
	if err != nil {
		return fmt.Errorf("secret: rewrap data key: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	return keyring.ErrConflict
}

// Destroy locks the key row, so a secret_values write that is checking the key (FOR SHARE in the trigger) either finishes first and is counted, or waits and then sees the key destroyed.
func (s *KeyStore) Destroy(ctx context.Context, id [16]byte, minAge time.Duration) error {
	return s.inTx(ctx, func(q *gen.Queries) error {
		k, err := q.LockSecretDataKeyForDestroy(ctx, gen.LockSecretDataKeyForDestroyParams{ID: uuid(id), MinAgeSeconds: minAge.Seconds()})
		if errors.Is(err, pgx.ErrNoRows) {
			return keyring.ErrUnknownKey
		}
		if err != nil {
			return fmt.Errorf("secret: lock data key: %w", err)
		}
		switch {
		case keyring.State(k.State) == keyring.Destroyed:
			return nil
		case keyring.State(k.State) == keyring.Primary:
			return keyring.ErrIsPrimary
		case minAge > 0 && !k.OldEnough:
			return keyring.ErrTooRecent
		}
		n, err := q.CountSecretValuesUsingDataKey(ctx, uuid(id))
		if err != nil {
			return fmt.Errorf("secret: count values under data key: %w", err)
		}
		if n > 0 {
			return keyring.ErrInUse
		}
		if err := q.DestroySecretDataKey(ctx, uuid(id)); err != nil {
			return fmt.Errorf("secret: destroy data key: %w", err)
		}
		return nil
	})
}

func (s *KeyStore) inTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("secret: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return keyring.ErrConflict
		}
		return fmt.Errorf("secret: commit: %w", err)
	}
	return nil
}

func dataKey(id pgtype.UUID, scope, state string, kek, wrapped []byte, created, demoted pgtype.Timestamptz) keyring.DataKey {
	k := keyring.DataKey{ID: id.Bytes, Scope: scope, State: keyring.State(state), Wrapped: wrapped, CreatedAt: created.Time}
	copy(k.KEKID[:], kek)
	if demoted.Valid {
		k.DemotedAt = demoted.Time
	}
	return k
}

func uuid(id [16]byte) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
