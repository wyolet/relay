// store.go is the data-access layer for ProviderKey. It is the only place
// that knows about encryption, the env-var lookup, or the split storage
// columns (value_kind / value_from_env / value_ciphertext / value_nonce).
//
// One Upsert routes to the right SQL based on Spec.ValueFrom.Kind. List
// reconstructs Spec from JSONB and populates the runtime-only Resolved /
// KeyHash fields by reading the env var or decrypting the ciphertext.
package providerkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
)

// Store is the ProviderKey data-access type. The 32-byte AES-GCM master key
// is plumbed at construction; pass nil to run env-only (any stored-mode
// operation errors loudly).
type Store struct {
	q         *gen.Queries
	masterKey []byte
}

// NewStore constructs a Store. masterKey may be nil if stored-mode keys are
// not used in this deployment.
func NewStore(q *gen.Queries, masterKey []byte) *Store {
	return &Store{q: q, masterKey: masterKey}
}

// List returns every ProviderKey row with Resolved + KeyHash populated.
// env-mode rows read os.Getenv; stored-mode rows are decrypted with the
// master key.
func (s *Store) List(ctx context.Context) ([]*ProviderKey, error) {
	rows, err := s.q.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("providerkey.List: %w", err)
	}
	out := make([]*ProviderKey, 0, len(rows))
	for _, r := range rows {
		k, err := s.fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("providerkey %s: %w", r.Name, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Upsert writes k. Routes to the env or stored SQL path based on
// Spec.ValueFrom.Kind. Cleartext Spec.Value is encrypted here and never
// reaches PG in cleartext; the field on k is cleared after a successful
// stored-mode write.
func (s *Store) Upsert(ctx context.Context, k *ProviderKey) error {
	metaJSON, err := meta.MarshalJSONB(k.Meta)
	if err != nil {
		return fmt.Errorf("providerkey.Upsert metadata: %w", err)
	}
	specJSON, err := marshalSpec(&k.Spec)
	if err != nil {
		return fmt.Errorf("providerkey.Upsert spec: %w", err)
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
		if len(s.masterKey) == 0 {
			return errors.New("providerkey.Upsert: stored mode requires master key")
		}
		if k.Spec.Value == "" {
			return errors.New("providerkey.Upsert: cleartext value required for stored mode")
		}
		ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(k.Spec.Value))
		if err != nil {
			return fmt.Errorf("providerkey.Upsert encrypt: %w", err)
		}
		_, err = s.q.InsertSecretStored(ctx, gen.InsertSecretStoredParams{
			ID:              k.Meta.ID,
			Name:            k.Meta.Name,
			DisplayName:     k.Meta.DisplayName,
			ValueCiphertext: ct,
			ValueNonce:      nonce,
			Metadata:        metaJSON,
			Spec:            specJSON,
		})
		if err != nil {
			return err
		}
		// Cleartext has been persisted as ciphertext; drop it from the struct.
		k.Spec.Value = ""
		return nil
	default:
		return fmt.Errorf("providerkey.Upsert: unknown value kind %q", k.Spec.ValueFrom.Kind)
	}
}

// Delete removes a ProviderKey by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteSecret(ctx, id)
}

func (s *Store) fromRow(r gen.ListSecretsRow) (*ProviderKey, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	k := &ProviderKey{Meta: md, Spec: spec}

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
		if len(s.masterKey) == 0 {
			return nil, errors.New("stored-mode row but master key not configured")
		}
		plain, err := crypto.Decrypt(s.masterKey, r.ValueCiphertext, r.ValueNonce)
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
