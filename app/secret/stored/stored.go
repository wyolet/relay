package stored

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/secret"
	"github.com/wyolet/relay/secret/aead"
	"github.com/wyolet/relay/secret/keyring"
)

var (
	_ secret.Resolver = (*Backend)(nil)
	_ secret.Writer   = (*Backend)(nil)
)

// Backend resolves and writes stored secrets: each value is sealed under its scope's data key with the row id as associated data. Rows from before the key ring (legacy: AES-GCM under the master key) still resolve and wake the rewriter. Resolve runs at snapshot load, on NOTIFY and in the key agent's heal, never per request.
type Backend struct {
	db   storage.DB
	q    *gen.Queries
	keys *keys
}

// WithDB returns a Backend that reads and writes through db, typically the caller's transaction, sharing the key ring with s.
func (s *Backend) WithDB(db storage.DB) *Backend {
	return &Backend{db: db, q: gen.New(db), keys: s.keys}
}

// Resolve returns the plaintext of a stored ref. It decrypts under the scope set with WithScope when there is one, else under the scope recorded on the row.
func (s *Backend) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindStored {
		return nil, fmt.Errorf("secret/stored: wrong kind %q", ref.Kind)
	}
	row, err := s.q.GetSecretValue(ctx, ref.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("secret/stored: %q not found", ref.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("secret/stored: get %q: %w", ref.ID, err)
	}
	scope, ok := scopeFrom(ctx)
	if !ok {
		scope = row.Scope.String
	}
	ring, err := s.keys.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("secret/stored: %w", err)
	}
	plain, st, err := openValue(ctx, ring, ref.ID, scope, row.Ciphertext, row.Nonce)
	if err != nil {
		return nil, fmt.Errorf("secret/stored: decrypt %q: %w", ref.ID, err)
	}
	if st.Stale {
		s.keys.wakeRewriter()
	}
	return plain, nil
}

// Create replaces the value of an existing stored secret, keeping its scope; it satisfies secret.Writer for callers that only update (the OAuth resolver persisting a rotated token). A new secret needs a scope: use CreateScoped.
func (s *Backend) Create(ctx context.Context, id string, plaintext []byte) (secret.Ref, error) {
	if id == "" {
		return secret.Ref{}, errors.New("secret/stored: id is required")
	}
	ring, err := s.keys.load(ctx)
	if err != nil {
		return secret.Ref{}, fmt.Errorf("secret/stored: %w", err)
	}
	// The scope is read under the locks an owner change takes, so the write cannot land in the scope the owner just left.
	err = s.inTx(ctx, func(b *Backend) error {
		v, err := b.lockValue(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("secret/stored: %q does not exist, and a new stored secret needs a scope", id)
		}
		if err != nil {
			return fmt.Errorf("secret/stored: get %q: %w", id, err)
		}
		if err := b.seal(ctx, ring, v.scope, id, plaintext); err != nil {
			return fmt.Errorf("secret/stored: write %q: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return secret.Ref{}, err
	}
	return secret.Ref{Kind: secret.KindStored, ID: id}, nil
}

// CreateScoped seals plaintext under scope's data key and writes it at id, replacing any value there.
func (s *Backend) CreateScoped(ctx context.Context, scope, id string, plaintext []byte) (secret.Ref, error) {
	if id == "" {
		return secret.Ref{}, errors.New("secret/stored: id is required")
	}
	ring, err := s.keys.load(ctx)
	if err != nil {
		return secret.Ref{}, fmt.Errorf("secret/stored: %w", err)
	}
	if err := s.seal(ctx, ring, scope, id, plaintext); err != nil {
		return secret.Ref{}, fmt.Errorf("secret/stored: write %q: %w", id, err)
	}
	return secret.Ref{Kind: secret.KindStored, ID: id}, nil
}

// Rescope moves a value to scope, decrypting the stored bytes and sealing them again, so an OAuth blob keeps its refresh token. Run it in the transaction that changes the owner. A legacy row is sealed under scope too, which changes its bytes so a rewrite that read it under the old owner finds it changed; one this process cannot decrypt is left for the rewriter of a process that can. A missing row and a sealed row already in scope are left alone.
func (s *Backend) Rescope(ctx context.Context, id, scope string) error {
	row, err := s.q.LockSecretValue(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secret/stored: get %q: %w", id, err)
	}
	legacy := !row.DekID.Valid
	if !legacy && row.Scope.String == scope {
		return nil
	}
	ring, err := s.keys.load(ctx)
	var plain []byte
	if err == nil {
		plain, _, err = openValue(ctx, ring, id, row.Scope.String, row.Ciphertext, row.Nonce)
	}
	if err != nil {
		if legacy {
			slog.Warn("secret: legacy stored secret left as it is on owner change; it cannot be decrypted here", "id", id, "err", err)
			return nil
		}
		return fmt.Errorf("secret/stored: rescope %q: %w", id, err)
	}
	if err := s.seal(ctx, ring, scope, id, plain); err != nil {
		return fmt.Errorf("secret/stored: rescope %q: %w", id, err)
	}
	return nil
}

// Delete removes the stored secret at id; deleting a missing id is not an error.
func (s *Backend) Delete(ctx context.Context, id string) error {
	return s.q.DeleteSecretValue(ctx, id)
}

// seal writes under a savepoint, so a write the trigger refuses (the key was demoted) leaves a caller's transaction usable for the ring's retry.
func (s *Backend) seal(ctx context.Context, ring *keyring.Ring, scope, id string, plaintext []byte) error {
	return ring.EncryptAndWrite(ctx, scope, plaintext, valueAAD(id), func(sealed []byte, keyID [16]byte) error {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = gen.New(tx).UpsertSealedSecretValue(ctx, gen.UpsertSealedSecretValueParams{
			ID: id, Ciphertext: sealed, Scope: pgtype.Text{String: scope, Valid: true}, DekID: uuid(keyID),
		})
		if err != nil {
			return writeError(err)
		}
		return writeError(tx.Commit(ctx))
	})
}

// lockedValue is a secret_values row read under its lock, with the scope its value belongs to.
type lockedValue struct {
	gen.LockSecretValueRow
	scope string
	owned bool
}

// lockValue locks the owning host key's row (shared) and then the value row, in the order host key writes take them, and derives the value's scope from what it read. It returns pgx.ErrNoRows when the value does not exist.
func (s *Backend) lockValue(ctx context.Context, id string) (lockedValue, error) {
	owner, err := s.q.LockSecretOwner(ctx, id)
	hasOwner := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return lockedValue{}, err
	}
	row, err := s.q.LockSecretValue(ctx, id)
	if err != nil {
		return lockedValue{}, err
	}
	scope, owned := rowScope(row.Scope, hasOwner, owner.OwnerKind, owner.OwnerID)
	return lockedValue{LockSecretValueRow: row, scope: scope, owned: owned}, nil
}

// inTx runs fn on a copy of s bound to a new transaction, a savepoint when s already runs in one, and commits when fn returns nil.
func (s *Backend) inTx(ctx context.Context, fn func(b *Backend) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("secret/stored: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.WithDB(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("secret/stored: commit: %w", err)
	}
	return nil
}

// openValue picks the row's shape from its bytes, not its columns, so a legacy value written behind a stale dek_id still opens as legacy.
func openValue(ctx context.Context, ring *keyring.Ring, id, scope string, ciphertext, nonce []byte) ([]byte, keyring.Status, error) {
	if !aead.IsSealed(ciphertext) {
		return ring.DecryptLegacy(ciphertext, nonce)
	}
	plain, st, err := ring.Decrypt(ctx, scope, ciphertext, valueAAD(id))
	if err != nil && len(nonce) > 0 {
		// A legacy value starts with the sealed magic by chance (about 2^-32 per row); only legacy rows carry a nonce.
		if lp, lst, lerr := ring.DecryptLegacy(ciphertext, nonce); lerr == nil {
			return lp, lst, nil
		}
	}
	return plain, st, err
}

// rowScope is the scope a row's value belongs to: the recorded one, else (a legacy row) its host key owner's, else system. owned is false for a legacy row nothing owns.
func rowScope(scope pgtype.Text, hasOwner bool, ownerKind, ownerID string) (s string, owned bool) {
	switch {
	case scope.Valid:
		return scope.String, true
	case hasOwner:
		return OwnerScope(ownerKind, ownerID), true
	}
	return ScopeSystem, false
}

// SQLSTATEs raised by the secret_values trigger (migration 34).
const (
	sqlstateNotPrimary  = "WSK01"
	sqlstateKeyMismatch = "WSK02"
	sqlstateLegacyWrite = "WSK03"
)

func writeError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case sqlstateNotPrimary:
		return fmt.Errorf("%w: %s", keyring.ErrNotPrimary, pgErr.Message)
	case sqlstateKeyMismatch:
		return fmt.Errorf("secret_values refused the row: %s", pgErr.Message)
	case sqlstateLegacyWrite:
		return fmt.Errorf("the database refuses legacy-shape secret writes: %s", pgErr.Message)
	}
	return err
}
