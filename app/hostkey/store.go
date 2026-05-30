// store.go is the data-access layer for HostKey. It owns the secrets-table
// rows (metadata/spec + value_kind/value_from_env) but delegates ALL secret
// material to pkg/secret: env lookups and stored AES-GCM ciphertext resolve
// through a secret.Registry, stored values are written via the
// StoredResolver into the generic secret_values table, and master-key
// rotation is the StoredResolver's job. This package no longer touches
// crypto or env directly.
//
// One Upsert routes to the env or stored path based on Spec.ValueFrom.Kind.
// List/Get reconstruct Spec from JSONB and populate the runtime-only
// Resolved/KeyHash fields by resolving the secret Ref.
package hostkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/secret"
)

// Store is the HostKey data-access type. resolver resolves env + stored
// refs to plaintext; stored is the write/rotate/version authority for the
// stored backend (nil for env-only deployments — stored-mode operations
// then error loudly).
type Store struct {
	q        *gen.Queries
	resolver *secret.Registry
	stored   *secret.StoredResolver
}

// NewStore constructs a Store. Pass the shared secret Registry + the stored
// resolver (see app/secret.Wire); stored may be nil to run env-only.
func NewStore(q *gen.Queries, resolver *secret.Registry, stored *secret.StoredResolver) *Store {
	return &Store{q: q, resolver: resolver, stored: stored}
}

// LoadKeyVersion aligns the stored resolver's in-memory key version to the
// maximum recorded in the store, so new secrets are tagged with the
// generation operators last rotated to. No-op when env-only.
func (s *Store) LoadKeyVersion(ctx context.Context) error {
	if s.stored == nil {
		return nil
	}
	return s.stored.LoadKeyVersion(ctx)
}

// KeyVersion returns the current master-key version (0 when env-only).
func (s *Store) KeyVersion() int32 {
	if s.stored == nil {
		return 0
	}
	return s.stored.KeyVersion()
}

// List returns every HostKey row with Resolved + KeyHash populated.
func (s *Store) List(ctx context.Context) ([]*HostKey, error) {
	rows, err := s.q.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("hostkey.List: %w", err)
	}
	out := make([]*HostKey, 0, len(rows))
	for _, r := range rows {
		k, err := s.fromRow(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("hostkey %s: %w", r.Name, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Upsert writes k. env rows store the var name; stored rows encrypt the
// cleartext into secret_values (via the stored resolver) and persist a
// ciphertext-free secrets row. Cleartext Spec.Value never reaches the
// secrets table and is cleared after a successful stored write.
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
		if s.stored == nil {
			return errors.New("hostkey.Upsert: stored mode requires a secret backend (master key)")
		}
		if k.Spec.Value == "" {
			return errors.New("hostkey.Upsert: cleartext value required for stored mode")
		}
		if _, err := s.stored.Create(ctx, k.Meta.ID, []byte(k.Spec.Value)); err != nil {
			return fmt.Errorf("hostkey.Upsert store secret: %w", err)
		}
		if _, err := s.q.InsertSecretStoredRef(ctx, gen.InsertSecretStoredRefParams{
			ID:          k.Meta.ID,
			Name:        k.Meta.Name,
			DisplayName: k.Meta.DisplayName,
			Metadata:    metaJSON,
			Spec:        specJSON,
		}); err != nil {
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
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
	k, err := s.fromRow(ctx, row)
	if err != nil {
		return nil, fmt.Errorf("hostkey.Get: %w", err)
	}
	return k, nil
}

// Delete removes a HostKey by id, plus its stored secret value (best-effort
// — orphaned ciphertext is harmless but we clean it up).
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.q.DeleteSecret(ctx, id); err != nil {
		return err
	}
	if s.stored != nil {
		if err := s.stored.Delete(ctx, id); err != nil {
			return fmt.Errorf("hostkey.Delete secret value: %w", err)
		}
	}
	return nil
}

// RotateResult is the outcome of a successful Rotate call.
type RotateResult struct {
	Rotated    int
	NewVersion int32
}

// Rotate re-encrypts every stored secret under newKey (transactionally, via
// the stored resolver) and swaps the live master key so the process keeps
// resolving without a restart. The caller persists newKey to the
// deployment's RELAY_MASTER_KEY for future boots.
func (s *Store) Rotate(ctx context.Context, newKey []byte) (RotateResult, error) {
	if s.stored == nil {
		return RotateResult{}, errors.New("hostkey.Rotate: no secret backend configured")
	}
	r, err := s.stored.Rotate(ctx, newKey)
	if err != nil {
		return RotateResult{}, err
	}
	return RotateResult{Rotated: r.Rotated, NewVersion: r.NewVersion}, nil
}

func (s *Store) fromRow(ctx context.Context, r gen.ListSecretsRow) (*HostKey, error) {
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
	k := &HostKey{Meta: md, Spec: spec}

	var ref secret.Ref
	switch r.ValueKind {
	case string(ValueKindEnv):
		k.Spec.ValueFrom.Kind = ValueKindEnv
		if r.ValueFromEnv.Valid {
			k.Spec.ValueFrom.Env = r.ValueFromEnv.String
		}
		if k.Spec.ValueFrom.Env == "" {
			return nil, errors.New("env-mode row missing value_from_env")
		}
		ref = secret.Ref{Kind: secret.KindEnv, Env: k.Spec.ValueFrom.Env}
	case string(ValueKindStored):
		k.Spec.ValueFrom.Kind = ValueKindStored
		ref = secret.Ref{Kind: secret.KindStored, ID: r.ID}
	default:
		return nil, fmt.Errorf("unknown value_kind %q", r.ValueKind)
	}

	plain, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve secret: %w", err)
	}
	k.Resolved = string(plain)
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
