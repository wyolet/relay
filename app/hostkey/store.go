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
	ver, ok, err := k.Meta.ExpectedVersion()
	if err != nil {
		return err
	}
	expected := pgtype.Int8{Int64: ver, Valid: ok}
	switch k.Spec.ValueFrom.Kind {
	case ValueKindEnv:
		_, err := s.q.InsertSecretEnv(ctx, gen.InsertSecretEnvParams{
			ID:              k.Meta.ID,
			Name:            k.Meta.Name,
			DisplayName:     k.Meta.DisplayName,
			ValueFromEnv:    pgtype.Text{String: k.Spec.ValueFrom.Env, Valid: true},
			Metadata:        metaJSON,
			Spec:            specJSON,
			ExpectedVersion: expected,
		})
		return staleIfNoRow(err)
	case ValueKindStored, ValueKindOAuth:
		// OAuth credentials share stored's at-rest path: the value (an OAuth
		// token blob for oauth mode) is AES-GCM-encrypted into secret_values and
		// the row records value_kind='stored'. The oauth semantics + provider
		// live in the spec JSON (ValueFrom.Kind/Provider), recovered in fromRow.
		if s.stored == nil {
			return errors.New("hostkey.Upsert: stored/oauth mode requires a secret backend (master key)")
		}
		// Without a new value this is a metadata-only update: the
		// secret_values ciphertext stays untouched. It is the only way to edit
		// an oauth key without re-supplying its token blob (its Resolved is the
		// access token, not the stored blob).
		if k.Spec.Value == "" && k.Resolved == "" {
			return errors.New("hostkey.Upsert: cleartext value required for stored/oauth mode")
		}
		writeRow := func() error {
			_, err := s.q.InsertSecretStoredRef(ctx, gen.InsertSecretStoredRefParams{
				ID:              k.Meta.ID,
				Name:            k.Meta.Name,
				DisplayName:     k.Meta.DisplayName,
				Metadata:        metaJSON,
				Spec:            specJSON,
				ExpectedVersion: expected,
			})
			return staleIfNoRow(err)
		}
		// A conditional write passes the version check before the value is
		// replaced, so a stale caller cannot rotate it. An unconditional one
		// keeps writing the value first: a failed create leaves no valueless row.
		if expected.Valid {
			if err := writeRow(); err != nil {
				return err
			}
		}
		if k.Spec.Value != "" {
			if _, err := s.stored.Create(ctx, k.Meta.ID, []byte(k.Spec.Value)); err != nil {
				return fmt.Errorf("hostkey.Upsert store secret: %w", err)
			}
		}
		if !expected.Valid {
			if err := writeRow(); err != nil {
				return err
			}
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
	k, err := s.fromRow(ctx, gen.ListSecretsRow(r))
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
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	k := &HostKey{Meta: md, Spec: spec}
	if len(r.Status) > 0 {
		if err := json.Unmarshal(r.Status, &k.Status); err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
	}

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
		// value_kind='stored' is the at-rest storage kind; the spec JSON says
		// whether it's a plain stored secret or an OAuth credential (whose
		// provider drives refresh). Distinguish on the spec, not the column.
		if spec.ValueFrom.Kind == ValueKindOAuth {
			k.Spec.ValueFrom.Kind = ValueKindOAuth
			ref = secret.Ref{Kind: secret.KindOAuth, ID: r.ID, Provider: spec.ValueFrom.Provider}
		} else {
			k.Spec.ValueFrom.Kind = ValueKindStored
			ref = secret.Ref{Kind: secret.KindStored, ID: r.ID}
		}
	default:
		return nil, fmt.Errorf("unknown value_kind %q", r.ValueKind)
	}

	plain, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		// One unreadable secret must not take the whole catalog down: the row
		// loads valueless and marked, so lists, edits and every other key keep
		// working while an operator fixes it.
		k.Status.Unresolved = &UnresolvedStatus{Reason: err.Error()}
		return k, nil
	}
	k.Resolved = string(plain)
	if k.Resolved != "" {
		sum := sha256.Sum256([]byte(k.Resolved))
		k.KeyHash = hex.EncodeToString(sum[:6])
	}
	return k, nil
}

// staleIfNoRow maps a conditional upsert that returned no row to
// meta.ErrStaleResourceVersion.
func staleIfNoRow(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return meta.ErrStaleResourceVersion
	}
	return err
}

// marshalSpec strips Value before serialising (defence in depth — the
// json:"-" tag already excludes it; this also catches accidental retags).
func marshalSpec(s *Spec) ([]byte, error) {
	cp := *s
	cp.Value = ""
	return json.Marshal(cp)
}

// SetCredentialStatus persists the observed credential state on the
// hostkey row (the refresher's renewed/revoked reports). A later value
// update (operator re-auth) clears it — see UpdateSecretStored.
func (s *Store) SetCredentialStatus(ctx context.Context, id string, cs CredentialStatus) error {
	st, err := json.Marshal(Status{Credential: &cs})
	if err != nil {
		return fmt.Errorf("hostkey.SetCredentialStatus: %w", err)
	}
	if err := s.q.UpdateSecretStatus(ctx, gen.UpdateSecretStatusParams{ID: id, Status: st}); err != nil {
		return fmt.Errorf("hostkey.SetCredentialStatus: %w", err)
	}
	return nil
}
