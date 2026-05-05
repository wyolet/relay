//go:build integration

package configstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	pgmigrations "github.com/wyolet/relay/migrations/postgres"
	"github.com/wyolet/relay/pkg/configstore"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("relay_test"),
		tcpostgres.WithUsername("relay"),
		tcpostgres.WithPassword("relay"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func runMigrations(t *testing.T, dsn string) {
	t.Helper()
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
}

// seedMinimal inserts the minimal valid catalog via direct SQL using pgx.
func seedMinimal(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()

	pool, err := configstore.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `
		INSERT INTO providers (name, metadata, spec) VALUES
		('ollama', '{"Name":"ollama","Labels":{}}', '{"kind":"ollama","baseURL":"http://localhost:11434","default":true}')
		ON CONFLICT DO NOTHING;

		INSERT INTO models (name, metadata, spec) VALUES
		('llama3', '{"Name":"llama3","Labels":{}}', '{"provider":"ollama","upstreamName":"llama3:8b","chat":true,"streaming":true}')
		ON CONFLICT DO NOTHING;

		INSERT INTO routes (name, metadata, spec) VALUES
		('default', '{"Name":"default","Labels":{}}', '{"default":true,"models":["llama3"]}')
		ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestPGStore_Boot_EmptyDB(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)

	// Empty catalog: boot should fail (validator requires ≥1 provider).
	_, err := configstore.Postgres(context.Background(), dsn)
	if err == nil {
		t.Fatal("expected error booting with empty catalog, got nil")
	}
}

func TestPGStore_Boot_And_Read(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	store, err := configstore.Postgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Postgres(): %v", err)
	}
	defer store.Close()

	if p, ok := store.ProviderByName("ollama"); !ok || p == nil {
		t.Error("ProviderByName(ollama) failed")
	}
	if m, ok := store.ModelByName("llama3"); !ok || m == nil {
		t.Error("ModelByName(llama3) failed")
	}
	if r, ok := store.RouteByName("default"); !ok || r == nil {
		t.Error("RouteByName(default) failed")
	}
	if def := store.DefaultProvider(); def == nil {
		t.Error("DefaultProvider() nil")
	}
	if def := store.DefaultRoute(); def == nil {
		t.Error("DefaultRoute() nil")
	}
	if ps := store.Providers(); len(ps) != 1 {
		t.Errorf("Providers(): got %d, want 1", len(ps))
	}
	if ms := store.Models(); len(ms) != 1 {
		t.Errorf("Models(): got %d, want 1", len(ms))
	}
}

func TestPGStore_Reload(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	store, err := configstore.Postgres(ctx, dsn)
	if err != nil {
		t.Fatalf("Postgres(): %v", err)
	}
	defer store.Close()

	// Insert a second provider via direct SQL.
	pool, err := configstore.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `
		INSERT INTO providers (name, metadata, spec) VALUES
		('ollama2', '{"Name":"ollama2","Labels":{}}', '{"kind":"ollama","baseURL":"http://localhost:11435"}')
		ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := store.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, ok := store.ProviderByName("ollama2"); !ok {
		t.Error("ProviderByName(ollama2) not found after Reload")
	}
	if ps := store.Providers(); len(ps) != 2 {
		t.Errorf("Providers() after reload: got %d, want 2", len(ps))
	}
}

func TestPGStore_MigrateIdempotent(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	// Second run must not error.
	runMigrations(t, dsn)
}

func TestPGStore_MalformedSpec(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)

	ctx := context.Background()
	pool, err := configstore.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Insert a provider with a spec where "kind" is a number (wrong type for ProviderKind string).
	// json.Unmarshal will error on type mismatch.
	_, err = pool.Exec(ctx, `
		INSERT INTO providers (name, metadata, spec) VALUES
		('bad', '{"Name":"bad"}', '{"kind":12345,"baseURL":"http://localhost"}');
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = configstore.Postgres(ctx, dsn)
	if err == nil {
		t.Fatal("expected error from malformed spec, got nil")
	}
}

func TestPGStore_ConcurrentReloadRace(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	store, err := configstore.Postgres(ctx, dsn)
	if err != nil {
		t.Fatalf("Postgres(): %v", err)
	}
	defer store.Close()

	deadline := time.Now().Add(500 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_ = store.Providers()
				_ = store.Reload(ctx)
				_, _ = store.ProviderByName("ollama")
				_ = store.Models()
			}
		}()
	}
	wg.Wait()
}

