//go:build integration

// tokenkey_concurrent_integration_test.go boots the signing-key generator from
// several goroutines against one Postgres. The read-generate-write is
// serialised by a real advisory lock, so there is no fake seam: without it
// each pod stores its own seed and the loser's tokens verify nowhere.
// Run with: make test-integration.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func setupTokenKeyDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// Four pods starting together must agree on one signing-key ref, and it
// must resolve to a usable Ed25519 seed — a second key would make every
// token the loser minted verify nowhere.
func TestIntegration_ConcurrentBootsGenerateOneSigningKey(t *testing.T) {
	pool, ctx := setupTokenKeyDB(t)
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{
		Pool: pool, MasterKey: masterKey,
	})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}

	// A first boot's state: tokens on, no key yet.
	raw, err := json.Marshal(settings.AuthTokens{
		Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("marshal section: %v", err)
	}
	if _, err := stores.Settings.Upsert(ctx, settings.AuthTokensSection, raw); err != nil {
		t.Fatalf("seed %s: %v", settings.AuthTokensSection, err)
	}

	const boots = 4
	secretID := "auth-tokens-signing-key-race"
	got := make([]*settings.AuthTokens, boots)
	errs := make([]error, boots)
	var wg sync.WaitGroup
	for i := 0; i < boots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = generateSigningKey(ctx, pool, stores, secretID, false)
		}(i)
	}
	wg.Wait()

	ref := ""
	for i, err := range errs {
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
		if got[i] == nil {
			t.Fatalf("boot %d returned no section", i)
		}
		if ref == "" {
			ref = got[i].SigningKey.ID
		}
		if got[i].SigningKey.ID != ref {
			t.Fatalf("boot %d recorded %q, boot 0 recorded %q — two keys were generated",
				i, got[i].SigningKey.ID, ref)
		}
	}
	if ref == "" {
		t.Fatal("no signing key ref was recorded")
	}

	// The stored section must agree with what every boot returned, and the
	// ref must resolve to a real seed — a lost write shows up as either.
	row, err := stores.Settings.Get(ctx, settings.AuthTokensSection)
	if err != nil {
		t.Fatalf("read back %s: %v", settings.AuthTokensSection, err)
	}
	stored, _ := row.Value.(*settings.AuthTokens)
	if stored == nil || stored.SigningKey.ID != ref {
		t.Fatalf("stored section = %+v, want signing key %q", stored, ref)
	}
	seed, err := stores.Secrets.Resolve(ctx, stored.SigningKey)
	if err != nil {
		t.Fatalf("resolve %q: %v", ref, err)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
}
