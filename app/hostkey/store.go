// store.go is the data-access layer for HostKey. It owns the secrets-table
// rows (metadata/spec + value_kind/value_from_env) but delegates ALL secret
// material: env lookups and stored ciphertext resolve through a
// secret.Registry, and stored values are written through the stored backend
// into secret_values, sealed under the scope of the key's owner. This package
// never touches crypto or env directly.
//
// One Upsert routes to the env or stored path based on Spec.ValueFrom.Kind.
// List/Get reconstruct Spec from JSONB and populate the runtime-only
// Resolved/KeyHash fields by resolving the secret Ref.
//
// Upsert is the one write path every caller (control API, apply, seed)
// shares, so it refuses a key that names no account: nothing makes one for it.

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

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/meta"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
	"github.com/wyolet/relay/secret"
)

// ErrNoSecretBackend is a stored or oauth value written while no secret backend (master key) is configured.
var ErrNoSecretBackend = errors.New("hostkey.Upsert: stored/oauth mode requires a secret backend (master key)")

// Store is the HostKey data-access type. resolver resolves env + stored
// refs to plaintext; stored writes stored-mode values (nil for env-only
// deployments — stored-mode operations then error loudly).
type Store struct {
	db       storage.DB
	q        *gen.Queries
	resolver *secret.Registry
	stored   *stored.Backend
	oauth    ValuePreparer
}

// ValuePreparer checks a new oauth value and returns what to store; see pkg/secret/oauth.Resolver.PrepareValue.
type ValuePreparer interface {
	PrepareValue(ctx context.Context, id string, raw []byte) ([]byte, error)
}

// NewStore constructs a Store over db. Pass the shared secret Registry + the
// stored backend (see app/secret.Wire); backend may be nil to run env-only.
// New oauth values are checked by a resolver with the default exchange client
// until UseOAuth names the one the registry resolves with.
func NewStore(db storage.DB, resolver *secret.Registry, backend *stored.Backend) *Store {
	return &Store{db: db, q: gen.New(db), resolver: resolver, stored: backend, oauth: pkgoauth.NewResolver(nil, nil)}
}

// WithDB returns a Store whose rows are read and written through db, typically the caller's transaction. Values resolve and new oauth values are checked as s's are; a new value is sealed through db, though its scope's data key may be made on the secret backend's own connection (see app/catalog.Stores.InTx).
func (s *Store) WithDB(db storage.DB) *Store {
	return &Store{db: db, q: gen.New(db), resolver: s.resolver, stored: s.stored, oauth: s.oauth}
}

// UseOAuth makes p check new oauth values.
func (s *Store) UseOAuth(p ValuePreparer) { s.oauth = p }

// SecretScope is the scope a key's stored value is sealed under: its owner.
func SecretScope(o meta.Owner) string {
	return stored.OwnerScope(string(o.Kind), o.ID)
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

// IDsInAccounts returns the ids of the host keys that name one of accountIDs, resolving no value.
func (s *Store) IDsInAccounts(ctx context.Context, accountIDs []string) ([]string, error) {
	want := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		want[id] = true
	}
	rows, err := s.q.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("hostkey.IDsInAccounts: %w", err)
	}
	var out []string
	for _, r := range rows {
		var spec struct {
			AccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			return nil, fmt.Errorf("hostkey %s: spec: %w", r.Name, err)
		}
		if want[spec.AccountID] {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// Upsert writes k. env rows store the var name; stored rows encrypt the
// cleartext into secret_values (via the stored resolver) and persist a
// ciphertext-free secrets row. Cleartext Spec.Value never reaches the
// secrets table and is cleared after a successful stored write. A key
// that names no account is refused with ErrNoAccount.
func (s *Store) Upsert(ctx context.Context, k *HostKey) error { return s.upsert(ctx, k, false) }

// ChangeOwner moves k, as read at its version, to owner: a stored or oauth value stays as stored, sealed again under the new owner's scope in the same transaction and decrypted before it commits; k's resolved value is not needed. A row changed since k was read, or a value that does not open, leaves the key as it was.
func (s *Store) ChangeOwner(ctx context.Context, k *HostKey, owner meta.Owner) error {
	k.Meta.Owner = owner
	k.Spec.Value = ""
	return s.upsert(ctx, k, true)
}

// UpsertKeepingValue writes k's stored or oauth row and no value: the value already sealed at k's id stays, moved to the scope of k's owner when sealed under another. A value sealed before the row's transaction (see app/catalog.Stores.InTx) is written with it.
func (s *Store) UpsertKeepingValue(ctx context.Context, k *HostKey) error {
	k.Spec.Value = ""
	return s.upsert(ctx, k, true)
}

// upsert is Upsert; keepValue writes no value and moves the stored one instead, as an edit carrying none does.
func (s *Store) upsert(ctx context.Context, k *HostKey, keepValue bool) error {
	if k.Spec.AccountID == "" {
		return fmt.Errorf("hostkey.Upsert %q: %w", k.Meta.Name, ErrNoAccount)
	}
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
		// token blob for oauth mode) is sealed into secret_values and
		// the row records value_kind='stored'. The oauth semantics + provider
		// live in the spec JSON (ValueFrom.Kind/Provider), recovered in fromRow.
		if s.stored == nil {
			return ErrNoSecretBackend
		}
		// Without a new value this is a metadata-only update: the stored
		// value is kept, only moved to the owner's scope if the owner changed.
		// It is the only way to edit an oauth key without re-supplying its
		// token blob (its Resolved is the access token, not the stored blob).
		if k.Spec.Value == "" && k.Resolved == "" && !keepValue && !k.KeepStoredValue {
			return errors.New("hostkey.Upsert: cleartext value required for stored/oauth mode")
		}
		if k.Spec.ValueFrom.Kind == ValueKindOAuth && k.Spec.Value != "" {
			prepared, err := s.oauth.PrepareValue(ctx, k.Meta.ID, []byte(k.Spec.Value))
			if err != nil {
				return &ValueRefusedError{Err: err}
			}
			k.Spec.Value = string(prepared)
		}
		scope := SecretScope(k.Meta.Owner)
		// The row and its value commit together, so a stale conditional write
		// replaces no value and a failed create leaves no valueless row.
		err := s.inTx(ctx, func(q *gen.Queries, backend *stored.Backend) error {
			_, err := q.InsertSecretStoredRef(ctx, gen.InsertSecretStoredRefParams{
				ID:              k.Meta.ID,
				Name:            k.Meta.Name,
				DisplayName:     k.Meta.DisplayName,
				Metadata:        metaJSON,
				Spec:            specJSON,
				ExpectedVersion: expected,
			})
			if err := staleIfNoRow(err); err != nil {
				return err
			}
			if k.Spec.Value == "" {
				return backend.Rescope(ctx, k.Meta.ID, scope)
			}
			if _, err := backend.CreateScoped(ctx, scope, k.Meta.ID, []byte(k.Spec.Value)); err != nil {
				return fmt.Errorf("hostkey.Upsert store secret: %w", err)
			}
			// A new credential starts with no renewal history, so a revoked one is renewed again.
			if k.Spec.ValueFrom.Kind == ValueKindOAuth {
				if err := q.UpdateSecretStatus(ctx, gen.UpdateSecretStatusParams{ID: k.Meta.ID}); err != nil {
					return fmt.Errorf("hostkey.Upsert clear status: %w", err)
				}
			}
			return nil
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

func (s *Store) inTx(ctx context.Context, fn func(q *gen.Queries, backend *stored.Backend) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("hostkey: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(gen.New(tx), s.stored.WithDB(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	if r.LastUsedAt.Valid {
		at := r.LastUsedAt.Time.UTC()
		k.Status.LastUsedAt = &at
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
		// A row stored before reserved names were refused must not send relay's configuration upstream; it loads unresolved instead.
		if reservedEnv(k.Spec.ValueFrom.Env) {
			k.Status.Unresolved = &UnresolvedStatus{Reason: ErrReservedEnv.Error()}
			return k, nil
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

	// The owner, not the row's scope column, decides the scope, so a value that missed a rescope or whose column was edited loads unresolved.
	plain, err := s.resolver.Resolve(stored.WithScope(ctx, SecretScope(md.Owner)), ref)
	if err != nil {
		// One unreadable secret must not take the whole catalog down: the row
		// loads valueless and marked, so lists, edits and every other key keep
		// working while an operator fixes it.
		k.Status.Unresolved = &UnresolvedStatus{Reason: err.Error()}
		return k, nil
	}
	k.Resolved = string(plain)
	if k.Resolved != "" {
		k.KeyHash = KeyHashOf(k.Resolved)
	}
	return k, nil
}

// KeyHashOf is the KeyHash of a key whose value is value.
func KeyHashOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
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
// hostkey row (the refresher's renewed/revoked reports). A later write of a
// new oauth value (operator re-auth) clears it.
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
