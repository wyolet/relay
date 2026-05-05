package configstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/db"
)

// PGStore implements ConfigStore backed by Postgres.
// Reads are served from an in-memory snapshot swapped atomically on Reload.
type PGStore struct {
	pool *pgxpool.Pool
	q    *db.Queries

	mu   sync.RWMutex
	snap *snapshot
}

// Postgres opens a connection pool and loads the initial catalog snapshot.
func Postgres(ctx context.Context, dsn string) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("configstore.Postgres: parse DSN: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("configstore.Postgres: open pool: %w", err)
	}

	s := &PGStore{pool: pool, q: db.New(pool)}
	if err := s.Reload(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the connection pool.
func (s *PGStore) Close() { s.pool.Close() }

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
	s := &PGStore{pool: pool, q: db.New(pool)}
	s.snap = snap
	return s, nil
}

// Reload re-reads the catalog from Postgres and atomically swaps the snapshot.
func (s *PGStore) Reload(ctx context.Context) error {
	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("configstore.PGStore.Reload: %w", err)
	}
	if err := validate(snap); err != nil {
		return fmt.Errorf("configstore.PGStore.Reload: catalog invalid: %w", err)
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
		resolveSecretPG(sec)
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

func resolveSecretPG(sec *Secret) {
	switch {
	case sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "":
		if val := os.Getenv(sec.Spec.ValueFrom.Env); val != "" {
			sec.Resolved = val
		}
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
}
