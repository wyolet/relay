//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	pgmigrations "github.com/wyolet/relay/migrations/postgres"
	"github.com/wyolet/relay/internal/catalog"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/crypto"
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

// seedMinimal inserts the minimal valid catalog via direct SQL.
func seedMinimal(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()

	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	if err := storagemod.SeedMinimalCatalog(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestPGStore_Boot_EmptyDB(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)

	// Empty catalog: boot should succeed — the relay starts with no config and
	// is populated via the admin API (M8 HITL use case).
	store, err := storagemod.Postgres(context.Background(), dsn, nil)
	if err != nil {
		t.Fatalf("Postgres() with empty catalog: %v", err)
	}
	defer store.Close()
	if got := store.Providers(); len(got) != 0 {
		t.Fatalf("expected 0 providers, got %d", len(got))
	}
}

func TestPGStore_Boot_And_Read(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	store, err := storagemod.Postgres(context.Background(), dsn, nil)
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
	store, err := storagemod.Postgres(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("Postgres(): %v", err)
	}
	defer store.Close()

	// Insert a second provider via the storage helper.
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	if err := storagemod.SeedProviderRow2(ctx, pool,
		"ollama2", `{"Name":"ollama2","Labels":{}}`, `{"kind":"ollama","baseURL":"http://localhost:11435"}`); err != nil {
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
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Insert a provider with a spec where "kind" is a number (wrong type for ProviderKind string).
	if err := storagemod.SeedMalformedProvider(ctx, pool); err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = storagemod.Postgres(ctx, dsn, nil)
	if err == nil {
		t.Fatal("expected error from malformed spec, got nil")
	}
}

func TestPGStore_ConcurrentReloadRace(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	store, err := storagemod.Postgres(ctx, dsn, nil)
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

// --- Migration 000002 tests ---

func runMigrationsUpDown(t *testing.T, dsn string) {
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
	if err := m.Down(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up (second): %v", err)
	}
}

func TestMigration000002_UpDownUp(t *testing.T) {
	dsn := startPostgres(t)
	runMigrationsUpDown(t, dsn)
}

func TestMigration000002_Backfill(t *testing.T) {
	dsn := startPostgres(t)

	// Run only migration 000001 so we can insert a legacy row.
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate steps(1): %v", err)
	}
	m.Close()

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Insert a legacy secret row using the old schema (no value_kind columns yet).
	if err := storagemod.SeedLegacySecretRow(ctx, pool); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Now run migration 000002.
	src2, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	m2, err := migrate.NewWithSourceInstance("iofs", src2, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	defer m2.Close()
	if err := m2.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up (000002): %v", err)
	}

	// Verify backfill.
	valueKind, valueFromEnv, err := storagemod.QuerySecretBackfill(ctx, pool, "legacy-env")
	if err != nil {
		t.Fatalf("query backfilled row: %v", err)
	}
	if valueKind != "env" {
		t.Errorf("value_kind = %q, want %q", valueKind, "env")
	}
	if valueFromEnv != "LEGACY_TEST_VAR" {
		t.Errorf("value_from_env = %q, want %q", valueFromEnv, "LEGACY_TEST_VAR")
	}
}

func TestMigration000002_CheckConstraintViolation(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Attempt to insert a row that violates the check constraint (env + ciphertext) — must fail.
	if err := storagemod.SeedBadConstraintSecret(ctx, pool); err == nil {
		t.Fatal("expected CHECK constraint violation, got nil")
	}
}

// --- Resolver tests ---

var testMasterKey = bytes.Repeat([]byte{0x42}, 32)

func seedMinimalWithSecrets(t *testing.T, dsn string) {
	t.Helper()
	seedMinimal(t, dsn)
}

func TestResolver_EnvMode_Set(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimalWithSecrets(t, dsn)

	t.Setenv("RELAY_SECRET_TESTVAR", "supersecret")

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}

	if err := store.UpsertSecretEnv(ctx, "test-env", "RELAY_SECRET_TESTVAR", "ollama", catalog.Metadata{Name: "test-env"}); err != nil {
		t.Fatalf("UpsertSecretEnv: %v", err)
	}

	// Boot a new PGStore which will resolve secrets.
	s, err := storagemod.Postgres(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("Postgres: %v", err)
	}
	defer s.Close()

	sec, ok := s.SecretByName("test-env")
	if !ok {
		t.Fatal("SecretByName(test-env) not found")
	}
	if sec.Resolved != "supersecret" {
		t.Errorf("Resolved = %q, want %q", sec.Resolved, "supersecret")
	}
}

func TestResolver_EnvMode_MissingVar(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}

	os.Unsetenv("RELAY_SECRET_MISSING")
	if err := store.UpsertSecretEnv(ctx, "missing-var", "RELAY_SECRET_MISSING", "ollama", catalog.Metadata{Name: "missing-var"}); err != nil {
		t.Fatalf("UpsertSecretEnv: %v", err)
	}

	_, err = storagemod.Postgres(ctx, dsn, nil)
	if err == nil {
		t.Fatal("expected error when env var missing, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolver_StoredMode_OK(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}
	store.SetMasterKey(testMasterKey)

	const plaintext = "my-api-key"
	if err := store.UpsertSecretStored(ctx, "stored-ok", plaintext, "ollama", catalog.Metadata{Name: "stored-ok"}); err != nil {
		t.Fatalf("UpsertSecretStored: %v", err)
	}

	s, err := storagemod.Postgres(ctx, dsn, testMasterKey)
	if err != nil {
		t.Fatalf("Postgres: %v", err)
	}
	defer s.Close()

	sec, ok := s.SecretByName("stored-ok")
	if !ok {
		t.Fatal("SecretByName(stored-ok) not found")
	}
	if sec.Resolved != plaintext {
		t.Errorf("Resolved = %q, want %q", sec.Resolved, plaintext)
	}
}

func TestResolver_StoredMode_NoMasterKey(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Insert stored-mode row directly (bypassing encryption layer).
	ct, nonce, err := crypto.Encrypt(testMasterKey, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := storagemod.SeedStoredSecret(ctx, pool, "stored-nokey", ct, nonce); err != nil {
		t.Fatalf("insert stored row: %v", err)
	}

	_, err = storagemod.Postgres(ctx, dsn, nil) // no master key
	if err == nil {
		t.Fatal("expected error when master key unset, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolver_StoredMode_TamperedCiphertext(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	ct, nonce, err := crypto.Encrypt(testMasterKey, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[0] ^= 0xFF // tamper

	if err := storagemod.SeedStoredSecret(ctx, pool, "stored-tampered", ct, nonce); err != nil {
		t.Fatalf("insert tampered row: %v", err)
	}

	_, err = storagemod.Postgres(ctx, dsn, testMasterKey)
	if err == nil {
		t.Fatal("expected error on tampered ciphertext, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// --- Write method tests ---

func TestUpsertSecretStored_EncryptsBeforeWrite(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}
	store.SetMasterKey(testMasterKey)

	const plaintext = "my-plaintext-key"
	if err := store.UpsertSecretStored(ctx, "enc-test", plaintext, "ollama", catalog.Metadata{Name: "enc-test"}); err != nil {
		t.Fatalf("UpsertSecretStored: %v", err)
	}

	// Read ciphertext from DB directly and verify it's NOT plaintext bytes.
	ct, nonce, err := storagemod.QuerySecretCiphertext(ctx, pool, "enc-test")
	if err != nil {
		t.Fatalf("query ciphertext: %v", err)
	}
	if bytes.Equal(ct, []byte(plaintext)) {
		t.Fatal("ciphertext equals plaintext — encryption did not happen")
	}

	// Round-trip decrypt.
	got, err := crypto.Decrypt(testMasterKey, ct, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decrypted = %q, want %q", got, plaintext)
	}
}

func TestUpsertSecretEnv_NoCiphertext(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}

	if err := store.UpsertSecretEnv(ctx, "env-test", "MY_ENV_VAR", "ollama", catalog.Metadata{Name: "env-test"}); err != nil {
		t.Fatalf("UpsertSecretEnv: %v", err)
	}

	valueKind, valueFromEnv, ct, err := storagemod.QuerySecretEnvRow(ctx, pool, "env-test")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if valueKind != "env" {
		t.Errorf("value_kind = %q, want env", valueKind)
	}
	if valueFromEnv != "MY_ENV_VAR" {
		t.Errorf("value_from_env = %q, want MY_ENV_VAR", valueFromEnv)
	}
	if ct != nil {
		t.Errorf("value_ciphertext should be NULL, got %v", ct)
	}
}

func TestDeleteSecret(t *testing.T) {
	dsn := startPostgres(t)
	runMigrations(t, dsn)
	seedMinimal(t, dsn)

	ctx := context.Background()
	pool, err := storagemod.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := storagemod.PostgresFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("PostgresFromPool: %v", err)
	}

	if err := store.UpsertSecretEnv(ctx, "del-test", "DEL_VAR", "ollama", catalog.Metadata{Name: "del-test"}); err != nil {
		t.Fatalf("UpsertSecretEnv: %v", err)
	}
	if err := store.DeleteSecret(ctx, "del-test"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	count, err := storagemod.CountSecrets(ctx, pool, "del-test")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after delete, got %d", count)
	}
}
