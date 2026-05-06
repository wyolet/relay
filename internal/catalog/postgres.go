package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
)

// PGStore implements Store backed by Postgres.
// Reads are served from an in-memory snapshot swapped atomically on Reload.
type PGStore struct {
	pool      *pgxpool.Pool
	q         *gen.Queries
	masterKey []byte // 32-byte AES-GCM key, nil when stored-mode secrets are not in use

	mu   sync.RWMutex
	snap *snapshot
}

// Postgres opens a connection pool and loads the initial catalog snapshot.
// masterKey is the parsed RELAY_MASTER_KEY (32 bytes) or nil.
func Postgres(ctx context.Context, dsn string, masterKey []byte) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog.Postgres: parse DSN: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("catalog.Postgres: open pool: %w", err)
	}

	s := &PGStore{pool: pool, q: gen.New(pool), masterKey: masterKey}
	if err := s.Reload(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the connection pool.
func (s *PGStore) Close() { s.pool.Close() }

// SetMasterKey sets the AES-GCM master key used for stored-mode secrets.
// Must be called before Reload or write methods that use stored mode.
func (s *PGStore) SetMasterKey(key []byte) { s.masterKey = key }

// RawPool returns the underlying pgxpool.Pool (for admin CRUD transaction management).
func (s *PGStore) RawPool() *pgxpool.Pool { return s.pool }

// HasMasterKey reports whether a master key is configured (stored-mode secrets are available).
func (s *PGStore) HasMasterKey() bool { return len(s.masterKey) > 0 }

// Ping checks the database connection.
func (s *PGStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// OpenPool opens a raw pgxpool.Pool with the same defaults as Postgres().
// Intended for tests and the seed CLI.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	return pgxpool.NewWithConfig(ctx, cfg)
}

// PostgresFromPool wraps an existing pgxpool.Pool into a PGStore without performing an initial Reload.
// The catalog snapshot is initially nil; callers must Reload before reading config.
// Intended for the seed CLI where the DB may be empty.
func PostgresFromPool(_ context.Context, pool *pgxpool.Pool) (*PGStore, error) {
	snap := newSnapshot()
	s := &PGStore{pool: pool, q: gen.New(pool), snap: snap}
	return s, nil
}

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
func (s *PGStore) RateLimitsForRequest(provider *Provider, pool *Pool, model *Model, secret *Secret) []ResolvedRule {
	return s.cur().rateLimitsForRequest(provider, pool, model, secret)
}

func (s *PGStore) loadSnapshot(ctx context.Context) (*snapshot, error) {
	snap := newSnapshot()

	provRows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListProviders: %w", err)
	}
	for _, row := range provRows {
		var meta Metadata
		var spec ProviderSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("provider %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("provider %q spec: %w", row.Name, err)
		}
		snap.providers[row.Name] = &Provider{APIVersion: APIVersion, Kind: KindProvider, Metadata: meta, Spec: spec}
	}

	poolRows, err := s.q.ListPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListPools: %w", err)
	}
	for _, row := range poolRows {
		var meta Metadata
		var spec PoolSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("pool %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("pool %q spec: %w", row.Name, err)
		}
		snap.pools[row.Name] = &Pool{APIVersion: APIVersion, Kind: KindPool, Metadata: meta, Spec: spec}
	}

	secRows, err := s.q.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListSecrets: %w", err)
	}
	for _, row := range secRows {
		var meta Metadata
		var spec SecretSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("secret %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("secret %q spec: %w", row.Name, err)
		}
		sec := &Secret{APIVersion: APIVersion, Kind: KindSecret, Metadata: meta, Spec: spec}
		if err := s.resolveSecretRow(sec, row.ValueKind, row.ValueFromEnv.String, row.ValueCiphertext, row.ValueNonce, row.ValueFromEnv.Valid); err != nil {
			return nil, err
		}
		snap.secrets[row.Name] = sec
	}

	modelRows, err := s.q.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListModels: %w", err)
	}
	for _, row := range modelRows {
		var meta Metadata
		var spec ModelSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("model %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("model %q spec: %w", row.Name, err)
		}
		snap.models[row.Name] = &Model{APIVersion: APIVersion, Kind: KindModel, Metadata: meta, Spec: spec}
	}

	routeRows, err := s.q.ListRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListRoutes: %w", err)
	}
	for _, row := range routeRows {
		var meta Metadata
		var spec RouteSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("route %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("route %q spec: %w", row.Name, err)
		}
		snap.routes[row.Name] = &Route{APIVersion: APIVersion, Kind: KindRoute, Metadata: meta, Spec: spec}
	}

	rlRows, err := s.q.ListRateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListRateLimits: %w", err)
	}
	for _, row := range rlRows {
		var meta Metadata
		var spec RateLimitSpec
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("ratelimit %q metadata: %w", row.Name, err)
		}
		if err := json.Unmarshal(row.Spec, &spec); err != nil {
			return nil, fmt.Errorf("ratelimit %q spec: %w", row.Name, err)
		}
		snap.rateLimits[row.Name] = &RateLimit{APIVersion: APIVersion, Kind: KindRateLimit, Metadata: meta, Spec: spec}
	}

	return snap, nil
}

// resolveSecretRow populates sec.Resolved from the DB columns.
// valueKindValid indicates whether valueFromEnv came from a non-NULL column.
func (s *PGStore) resolveSecretRow(sec *Secret, valueKind, valueFromEnvStr string, valueCiphertext, valueNonce []byte, valueFromEnvValid bool) error {
	name := sec.Metadata.Name
	switch valueKind {
	case "env":
		if !valueFromEnvValid {
			// Legacy row without value_from_env set (pre-migration or malformed).
			// Fall through to spec-based resolution.
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
		// value_kind column not yet populated (pre-000002 row via UpsertSecret).
		return s.resolveSecretSpec(sec)
	}
	if sec.Resolved != "" {
		sum := sha256.Sum256([]byte(sec.Resolved))
		sec.KeyHash = fmt.Sprintf("%x", sum[:6])
	}
	return nil
}

// resolveSecretSpec handles legacy rows that were written before migration 000002
// (i.e. UpsertSecret path that doesn't populate value_kind columns).
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
// provider is the provider name (e.g. "ollama") that this secret belongs to.
func (s *PGStore) UpsertSecretEnv(ctx context.Context, tx pgx.Tx, name, envVar, provider string, meta Metadata) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("UpsertSecretEnv: marshal metadata: %w", err)
	}
	specJSON, err := json.Marshal(SecretSpec{Provider: provider, ValueFrom: &SecretValueFrom{Env: envVar}})
	if err != nil {
		return fmt.Errorf("UpsertSecretEnv: marshal spec: %w", err)
	}
	q := s.q
	if tx != nil {
		q = gen.New(tx)
	}
	_, err = q.InsertSecretEnv(ctx, gen.InsertSecretEnvParams{
		Name:         name,
		ValueFromEnv: pgtype.Text{String: envVar, Valid: true},
		Metadata:     metaJSON,
		Spec:         specJSON,
	})
	return err
}

// UpsertSecretStored inserts or updates a secret in stored (encrypted) mode.
// plaintext is encrypted with s.masterKey before writing.
// provider is the provider name (e.g. "ollama") that this secret belongs to.
func (s *PGStore) UpsertSecretStored(ctx context.Context, tx pgx.Tx, name, plaintext, provider string, meta Metadata) error {
	if len(s.masterKey) == 0 {
		return fmt.Errorf("UpsertSecretStored: stored mode requires RELAY_MASTER_KEY")
	}
	ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("UpsertSecretStored: encrypt: %w", err)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("UpsertSecretStored: marshal metadata: %w", err)
	}
	specJSON, err := json.Marshal(SecretSpec{Provider: provider})
	if err != nil {
		return fmt.Errorf("UpsertSecretStored: marshal spec: %w", err)
	}
	q := s.q
	if tx != nil {
		q = gen.New(tx)
	}
	_, err = q.InsertSecretStored(ctx, gen.InsertSecretStoredParams{
		Name:            name,
		ValueCiphertext: ct,
		ValueNonce:      nonce,
		Metadata:        metaJSON,
		Spec:            specJSON,
	})
	return err
}

// UpdateSecretEnv changes an existing secret to env-ref mode.
func (s *PGStore) UpdateSecretEnv(ctx context.Context, tx pgx.Tx, name, envVar string) error {
	q := s.q
	if tx != nil {
		q = gen.New(tx)
	}
	_, err := q.UpdateSecretEnv(ctx, gen.UpdateSecretEnvParams{Name: name, ValueFromEnv: pgtype.Text{String: envVar, Valid: true}})
	return err
}

// UpdateSecretStored rotates the ciphertext for a stored-mode secret.
func (s *PGStore) UpdateSecretStored(ctx context.Context, tx pgx.Tx, name, plaintext string) error {
	if len(s.masterKey) == 0 {
		return fmt.Errorf("UpdateSecretStored: stored mode requires RELAY_MASTER_KEY")
	}
	ct, nonce, err := crypto.Encrypt(s.masterKey, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("UpdateSecretStored: encrypt: %w", err)
	}
	q := s.q
	if tx != nil {
		q = gen.New(tx)
	}
	_, err = q.UpdateSecretStored(ctx, gen.UpdateSecretStoredParams{
		Name:            name,
		ValueCiphertext: ct,
		ValueNonce:      nonce,
	})
	return err
}

// DeleteSecret removes a secret by name.
func (s *PGStore) DeleteSecret(ctx context.Context, tx pgx.Tx, name string) error {
	q := s.q
	if tx != nil {
		q = gen.New(tx)
	}
	return q.DeleteSecret(ctx, name)
}
