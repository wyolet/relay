// store.go is the data-access layer for HostKey. It is the only place
// that knows about encryption, the env-var lookup, or the split storage
// columns (value_kind / value_from_env / value_ciphertext / value_nonce /
// value_key_version).
//
// One Upsert routes to the right SQL based on Spec.ValueFrom.Kind. List
// reconstructs Spec from JSONB and populates the runtime-only Resolved /
// KeyHash fields by reading the env var or decrypting the ciphertext.
//
// Rotate performs an in-place re-encryption of every stored-mode row with
// a new master key, within a single transaction. The Store's in-memory
// masterKey and keyVersion are mutated on success so subsequent operations
// in the same process use the new key without restart.
package hostkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
)

// Store is the HostKey data-access type. The 32-byte AES-GCM master key
// is plumbed at construction; pass nil to run env-only (any stored-mode
// operation errors loudly). The pool is required for Rotate's tx; pass
// nil only in tests that don't exercise Rotate.
type Store struct {
	q    *gen.Queries
	pool *pgxpool.Pool

	mu         sync.RWMutex
	masterKey  []byte
	keyVersion int32
}

// NewStore constructs a Store. masterKey may be nil if stored-mode keys are
// not used. The initial keyVersion is 1; call LoadKeyVersion(ctx) after
// construction to align it with any prior rotations recorded in PG.
func NewStore(q *gen.Queries, pool *pgxpool.Pool, masterKey []byte) *Store {
	return &Store{q: q, pool: pool, masterKey: masterKey, keyVersion: 1}
}

// LoadKeyVersion reads MAX(value_key_version) from stored rows and sets the
// Store's in-memory version to that value, so new Upserts tag their rows
// with the same generation operators last rotated to. Safe to call on an
// empty table — leaves keyVersion at its current value.
func (s *Store) LoadKeyVersion(ctx context.Context) error {
	if s.pool == nil {
		return nil
	}
	var v pgtype.Int4
	err := s.pool.QueryRow(ctx,
		`SELECT MAX(value_key_version) FROM secrets WHERE value_kind = 'stored'`,
	).Scan(&v)
	if err != nil {
		return fmt.Errorf("hostkey.LoadKeyVersion: %w", err)
	}
	if v.Valid && v.Int32 > 0 {
		s.mu.Lock()
		s.keyVersion = v.Int32
		s.mu.Unlock()
	}
	return nil
}

// KeyVersion returns the current in-memory master-key version.
func (s *Store) KeyVersion() int32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyVersion
}

// List returns every HostKey row with Resolved + KeyHash populated.
// env-mode rows read os.Getenv; stored-mode rows are decrypted with the
// master key.
func (s *Store) List(ctx context.Context) ([]*HostKey, error) {
	rows, err := s.q.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("hostkey.List: %w", err)
	}
	out := make([]*HostKey, 0, len(rows))
	for _, r := range rows {
		k, err := s.fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("hostkey %s: %w", r.Name, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Upsert writes k. Routes to the env or stored SQL path based on
// Spec.ValueFrom.Kind. Cleartext Spec.Value is encrypted here and never
// reaches PG in cleartext; the field on k is cleared after a successful
// stored-mode write.
func (s *Store) Upsert(ctx context.Context, k *HostKey) error {
	metaJSON, err := meta.MarshalJSONB(k.Meta)
	if err != nil {
		return fmt.Errorf("hostkey.Upsert metadata: %w", err)
	}
	specJSON, err := marshalSpec(&k.Spec)
	if err != nil {
		return fmt.Errorf("hostkey.Upsert spec: %w", err)
	}
	switch k.Spec.ValueFrom.Kind {
	case ValueKindEnv:
		_, err := s.q.InsertSecretEnv(ctx, gen.InsertSecretEnvParams{
			ID:           k.Meta.ID,
			Name:         k.Meta.Name,
			DisplayName:  k.Meta.DisplayName,
			ValueFromEnv: pgtype.Text{String: k.Spec.ValueFrom.Env, Valid: true},
			Metadata:     metaJSON,
			Spec:         specJSON,
		})
		return err
	case ValueKindStored:
		s.mu.RLock()
		mk := s.masterKey
		ver := s.keyVersion
		s.mu.RUnlock()
		if len(mk) == 0 {
			return errors.New("hostkey.Upsert: stored mode requires master key")
		}
		if k.Spec.Value == "" {
			return errors.New("hostkey.Upsert: cleartext value required for stored mode")
		}
		ct, nonce, err := crypto.Encrypt(mk, []byte(k.Spec.Value))
		if err != nil {
			return fmt.Errorf("hostkey.Upsert encrypt: %w", err)
		}
		_, err = s.q.InsertSecretStored(ctx, gen.InsertSecretStoredParams{
			ID:              k.Meta.ID,
			Name:            k.Meta.Name,
			DisplayName:     k.Meta.DisplayName,
			ValueCiphertext: ct,
			ValueNonce:      nonce,
			ValueKeyVersion: pgtype.Int4{Int32: ver, Valid: true},
			Metadata:        metaJSON,
			Spec:            specJSON,
		})
		if err != nil {
			return err
		}
		k.Spec.Value = ""
		return nil
	default:
		return fmt.Errorf("hostkey.Upsert: unknown value kind %q", k.Spec.ValueFrom.Kind)
	}
}

// Get returns the HostKey with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*HostKey, error) {
	r, err := s.q.GetSecret(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("hostkey.Get: %w", err)
	}
	row := gen.ListSecretsRow{
		ID:              r.ID,
		Name:            r.Name,
		DisplayName:     r.DisplayName,
		Metadata:        r.Metadata,
		Spec:            r.Spec,
		ValueKind:       r.ValueKind,
		ValueFromEnv:    r.ValueFromEnv,
		ValueCiphertext: r.ValueCiphertext,
		ValueNonce:      r.ValueNonce,
		ValueKeyVersion: r.ValueKeyVersion,
	}
	k, err := s.fromRow(row)
	if err != nil {
		return nil, fmt.Errorf("hostkey.Get: %w", err)
	}
	return k, nil
}

// Delete removes a HostKey by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteSecret(ctx, id)
}

// RotateResult is the outcome of a successful Rotate call.
type RotateResult struct {
	// Rotated is the number of stored-mode rows re-encrypted.
	Rotated int
	// NewVersion is the value_key_version assigned to every rotated row.
	NewVersion int32
}

// Rotate generates ciphertext for every stored-mode row using newKey and
// commits the updates in a single transaction. On success the Store's
// in-memory masterKey is swapped to newKey and keyVersion is bumped, so
// subsequent Upserts and Lists use the new key without a process restart.
//
// The caller is responsible for persisting newKey to the deployment's
// RELAY_MASTER_KEY env so future process boots can decrypt the rotated
// rows.
func (s *Store) Rotate(ctx context.Context, newKey []byte) (RotateResult, error) {
	if s.pool == nil {
		return RotateResult{}, errors.New("hostkey.Rotate: pool not configured")
	}
	if len(newKey) != 32 {
		return RotateResult{}, fmt.Errorf("hostkey.Rotate: newKey must be 32 bytes, got %d", len(newKey))
	}
	s.mu.RLock()
	oldKey := s.masterKey
	oldVer := s.keyVersion
	s.mu.RUnlock()
	if len(oldKey) == 0 {
		return RotateResult{}, errors.New("hostkey.Rotate: current master key not configured")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RotateResult{}, fmt.Errorf("hostkey.Rotate begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)
	rows, err := qtx.ListStoredSecretsForRotation(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("hostkey.Rotate list: %w", err)
	}
	newVer := oldVer + 1
	for _, r := range rows {
		plain, err := crypto.Decrypt(oldKey, r.ValueCiphertext, r.ValueNonce)
		if err != nil {
			return RotateResult{}, fmt.Errorf("hostkey.Rotate decrypt %s: %w", r.ID, err)
		}
		ct, nonce, err := crypto.Encrypt(newKey, plain)
		if err != nil {
			return RotateResult{}, fmt.Errorf("hostkey.Rotate encrypt %s: %w", r.ID, err)
		}
		if err := qtx.UpdateSecretCiphertext(ctx, gen.UpdateSecretCiphertextParams{
			ID:              r.ID,
			ValueCiphertext: ct,
			ValueNonce:      nonce,
			ValueKeyVersion: pgtype.Int4{Int32: newVer, Valid: true},
		}); err != nil {
			return RotateResult{}, fmt.Errorf("hostkey.Rotate update %s: %w", r.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RotateResult{}, fmt.Errorf("hostkey.Rotate commit: %w", err)
	}

	s.mu.Lock()
	s.masterKey = newKey
	s.keyVersion = newVer
	s.mu.Unlock()

	return RotateResult{Rotated: len(rows), NewVersion: newVer}, nil
}

func (s *Store) fromRow(r gen.ListSecretsRow) (*HostKey, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	k := &HostKey{Meta: md, Spec: spec}

	switch r.ValueKind {
	case string(ValueKindEnv):
		k.Spec.ValueFrom.Kind = ValueKindEnv
		if r.ValueFromEnv.Valid {
			k.Spec.ValueFrom.Env = r.ValueFromEnv.String
		}
		if k.Spec.ValueFrom.Env == "" {
			return nil, errors.New("env-mode row missing value_from_env")
		}
		v, ok := os.LookupEnv(k.Spec.ValueFrom.Env)
		if !ok || v == "" {
			return nil, fmt.Errorf("env var %q not set", k.Spec.ValueFrom.Env)
		}
		k.Resolved = v
	case string(ValueKindStored):
		k.Spec.ValueFrom.Kind = ValueKindStored
		s.mu.RLock()
		mk := s.masterKey
		s.mu.RUnlock()
		if len(mk) == 0 {
			return nil, errors.New("stored-mode row but master key not configured")
		}
		plain, err := crypto.Decrypt(mk, r.ValueCiphertext, r.ValueNonce)
		if err != nil {
			return nil, fmt.Errorf("decrypt: %w", err)
		}
		k.Resolved = string(plain)
	default:
		return nil, fmt.Errorf("unknown value_kind %q", r.ValueKind)
	}
	if k.Resolved != "" {
		sum := sha256.Sum256([]byte(k.Resolved))
		k.KeyHash = hex.EncodeToString(sum[:6])
	}
	return k, nil
}

// marshalSpec strips Value before serialising (defence in depth — the
// json:"-" tag already excludes it; this also catches accidental retags).
func marshalSpec(s *Spec) ([]byte, error) {
	cp := *s
	cp.Value = ""
	return json.Marshal(cp)
}
