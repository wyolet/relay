package catalog

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/wyolet/relay/pkg/crypto"
)

// Pinger is anything that can report database connectivity.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Closer is anything with a Close method (e.g. a connection pool).
type Closer interface {
	Close()
}

// PGStore implements Store backed by Postgres via CatalogDB.
// Reads are served from an in-memory snapshot swapped atomically on Reload.
// PGStore does not import internal/storage; it depends only on the CatalogDB
// and TxRunner interfaces defined in this package, which storage satisfies.
type PGStore struct {
	db        CatalogDB
	tx        TxRunner
	masterKey []byte // 32-byte AES-GCM key, nil when stored-mode secrets are not in use

	pinger Pinger
	closer Closer

	mu   sync.RWMutex
	snap *snapshot
}

// Ping delegates to the underlying Pinger (e.g. *storage.Storage).
// Returns nil if no pinger is configured.
func (s *PGStore) Ping(ctx context.Context) error {
	if s.pinger == nil {
		return nil
	}
	return s.pinger.Ping(ctx)
}

// Close delegates to the underlying Closer (e.g. *storage.Storage).
// No-op if no closer is configured.
func (s *PGStore) Close() {
	if s.closer != nil {
		s.closer.Close()
	}
}

// NewPGStore constructs a PGStore from a CatalogDB + TxRunner, then loads the
// initial snapshot. masterKey is the parsed RELAY_MASTER_KEY (32 bytes) or nil.
// If tx also implements Pinger and/or Closer, those are wired automatically.
func NewPGStore(db CatalogDB, tx TxRunner, masterKey []byte) (*PGStore, error) {
	ps := &PGStore{db: db, tx: tx, masterKey: masterKey}
	if p, ok := tx.(Pinger); ok {
		ps.pinger = p
	}
	if c, ok := tx.(Closer); ok {
		ps.closer = c
	}
	ctx := context.Background()
	if err := ps.Reload(ctx); err != nil {
		return nil, err
	}
	return ps, nil
}

// NewPGStoreNoReload constructs a PGStore without an initial Reload.
// The snapshot is initially empty; callers must Reload before reading config.
// Intended for the seed CLI and tests where the DB may be empty.
func NewPGStoreNoReload(db CatalogDB, tx TxRunner) (*PGStore, error) {
	snap := newSnapshot()
	ps := &PGStore{db: db, tx: tx, snap: snap}
	if p, ok := tx.(Pinger); ok {
		ps.pinger = p
	}
	if c, ok := tx.(Closer); ok {
		ps.closer = c
	}
	return ps, nil
}

// SetMasterKey sets the AES-GCM master key used for stored-mode secrets.
func (s *PGStore) SetMasterKey(key []byte) { s.masterKey = key }

// HasMasterKey reports whether a master key is configured.
func (s *PGStore) HasMasterKey() bool { return len(s.masterKey) > 0 }

// Reload re-reads the catalog from Postgres and atomically swaps the snapshot.
func (s *PGStore) Reload(ctx context.Context) error {
	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("catalog.PGStore.Reload: %w", err)
	}
	if err := validate(snap); err != nil {
		return fmt.Errorf("catalog.PGStore.Reload: catalog invalid: %w", err)
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return nil
}

func (s *PGStore) cur() *snapshot {
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()
	return snap
}

func (s *PGStore) ProviderByName(name string) (*Provider, bool)  { return s.cur().providerByName(name) }
func (s *PGStore) ModelByName(name string) (*Model, bool)         { return s.cur().modelByName(name) }
func (s *PGStore) RouteByName(name string) (*Route, bool)         { return s.cur().routeByName(name) }
func (s *PGStore) RateLimitByName(name string) (*RateLimit, bool) { return s.cur().rateLimitByName(name) }
func (s *PGStore) SecretByName(name string) (*Secret, bool)       { return s.cur().secretByName(name) }
func (s *PGStore) PoolByName(name string) (*Pool, bool)           { return s.cur().poolByName(name) }
func (s *PGStore) Providers() []*Provider                         { return s.cur().listProviders() }
func (s *PGStore) Models() []*Model                               { return s.cur().listModels() }
func (s *PGStore) Routes() []*Route                               { return s.cur().listRoutes() }
func (s *PGStore) RateLimits() []*RateLimit                       { return s.cur().listRateLimits() }
func (s *PGStore) Secrets() []*Secret                             { return s.cur().listSecrets() }
func (s *PGStore) Pools() []*Pool                                 { return s.cur().listPools() }
func (s *PGStore) DefaultProvider() *Provider                     { return s.cur().defaultProvider() }
func (s *PGStore) DefaultRoute() *Route                           { return s.cur().defaultRoute() }
func (s *PGStore) ProviderForModel(modelName string) (*Provider, bool) {
	return s.cur().providerForModel(modelName)
}
func (s *PGStore) SecretsForPool(p *Pool) []*Secret { return s.cur().secretsForPool(p) }
func (s *PGStore) RateLimitsForRequest(provider *Provider, pool *Pool, model *Model, sec *Secret) []ResolvedRule {
	return s.cur().rateLimitsForRequest(provider, pool, model, sec)
}

func (s *PGStore) loadSnapshot(ctx context.Context) (*snapshot, error) {
	snap := newSnapshot()

	provs, err := s.db.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListProviders: %w", err)
	}
	for _, p := range provs {
		pp := p
		snap.providers[p.Metadata.Name] = &pp
	}

	pools, err := s.db.ListPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListPools: %w", err)
	}
	for _, p := range pools {
		pp := p
		snap.pools[p.Metadata.Name] = &pp
	}

	secRows, err := s.db.ListSecretRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListSecrets: %w", err)
	}
	for _, row := range secRows {
		sec := &Secret{
			APIVersion: APIVersion,
			Kind:       KindSecret,
			Metadata:   row.Metadata,
			Spec:       row.Spec,
		}
		if err := s.resolveSecretRow(sec, row.ValueKind, row.ValueFromEnv, row.ValueFromEnvSet, row.ValueCiphertext, row.ValueNonce); err != nil {
			return nil, err
		}
		snap.secrets[row.Name] = sec
	}

	models, err := s.db.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListModels: %w", err)
	}
	for _, m := range models {
		mm := m
		snap.models[m.Metadata.Name] = &mm
	}

	routes, err := s.db.ListRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListRoutes: %w", err)
	}
	for _, r := range routes {
		rr := r
		snap.routes[r.Metadata.Name] = &rr
	}

	rls, err := s.db.ListRateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListRateLimits: %w", err)
	}
	for _, rl := range rls {
		rrl := rl
		snap.rateLimits[rl.Metadata.Name] = &rrl
	}

	return snap, nil
}

func (s *PGStore) resolveSecretRow(sec *Secret, valueKind, valueFromEnvStr string, valueFromEnvSet bool, valueCiphertext, valueNonce []byte) error {
	name := sec.Metadata.Name
	switch valueKind {
	case "env":
		if !valueFromEnvSet {
			return s.resolveSecretSpec(sec)
		}
		val := os.Getenv(valueFromEnvStr)
		if val == "" {
			return fmt.Errorf("secret %s: env var %s not set", name, valueFromEnvStr)
		}
		sec.Resolved = val
	case "stored":
		if len(s.masterKey) == 0 {
			return fmt.Errorf("secret %s: stored mode requires RELAY_MASTER_KEY", name)
		}
		plain, err := crypto.Decrypt(s.masterKey, valueCiphertext, valueNonce)
		if err != nil {
			return fmt.Errorf("secret %s: decrypt failed: %w", name, err)
		}
		sec.Resolved = string(plain)
	default:
		return s.resolveSecretSpec(sec)
	}
	if sec.Resolved != "" {
		sum := sha256.Sum256([]byte(sec.Resolved))
		sec.KeyHash = fmt.Sprintf("%x", sum[:6])
	}
	return nil
}

func (s *PGStore) resolveSecretSpec(sec *Secret) error {
	switch {
	case sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "":
		val := os.Getenv(sec.Spec.ValueFrom.Env)
		if val == "" {
			return fmt.Errorf("secret %s: env var %s not set", sec.Metadata.Name, sec.Spec.ValueFrom.Env)
		}
		sec.Resolved = val
	case sec.Spec.Value != "":
		sec.Resolved = sec.Spec.Value
		sec.UsedLiteral = true
		slog.Warn("secret uses literal value (deprecated)", "secret", sec.Metadata.Name)
	default:
		sec.UsedLiteral = true
	}
	if sec.Resolved != "" {
		sum := sha256.Sum256([]byte(sec.Resolved))
		sec.KeyHash = fmt.Sprintf("%x", sum[:6])
	}
	return nil
}

// UpsertSecretEnv inserts or updates a secret in env-ref mode.
func (s *PGStore) UpsertSecretEnv(ctx context.Context, name, envVar, provider string, meta Metadata) error {
	return s.db.UpsertSecretEnv(ctx, name, envVar, provider, meta)
}

// UpsertSecretStored inserts or updates a secret in stored (encrypted) mode.
// plaintext is encrypted here with s.masterKey before passing to storage.
func (s *PGStore) UpsertSecretStored(ctx context.Context, name, plaintext, provider string, meta Metadata) error {
	if len(s.masterKey) == 0 {
		return fmt.Errorf("UpsertSecretStored: stored mode requires RELAY_MASTER_KEY")
	}
	ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("UpsertSecretStored: encrypt: %w", err)
	}
	return s.db.UpsertSecretStored(ctx, name, provider, meta, ct, nonce)
}

// UpdateSecretEnv changes an existing secret to env-ref mode.
func (s *PGStore) UpdateSecretEnv(ctx context.Context, name, envVar string) error {
	return s.db.UpdateSecretEnv(ctx, name, envVar)
}

// UpdateSecretStored rotates the ciphertext for a stored-mode secret.
func (s *PGStore) UpdateSecretStored(ctx context.Context, name, plaintext string) error {
	if len(s.masterKey) == 0 {
		return fmt.Errorf("UpdateSecretStored: stored mode requires RELAY_MASTER_KEY")
	}
	ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("UpdateSecretStored: encrypt: %w", err)
	}
	return s.db.UpdateSecretStored(ctx, name, ct, nonce)
}

// DeleteSecret removes a secret by name.
func (s *PGStore) DeleteSecret(ctx context.Context, name string) error {
	return s.db.DeleteSecret(ctx, name)
}
