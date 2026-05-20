//go:build integration

// integration_test.go exercises the end-to-end catalog stack against a
// real Postgres. Run with: make test-integration  (spins up an ephemeral
// pg via deploy/compose/docker-compose.test.yml).
//
// What it covers:
//   - Migrations 0001..0011 apply cleanly to a fresh DB.
//   - Bootstrap wires every store; initial Reload over an empty DB succeeds.
//   - Direct stores.X.Upsert writes flow through NOTIFY → Listener →
//     debouncer → Apply* and become visible in the Snapshot within ~1.5s.
//   - Cascade: deleting a Model evicts a dependent Pricing from snapshot.
//
// This is the single test that catches regressions across boot, listener,
// debouncer, reconciler, and reverse-ref cascade — all the PG-touching
// paths the unit tests can't exercise.
package catalog

import (
	"github.com/wyolet/relay/app/adapters"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/internal/storage/gen"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

const flushPad = 1500 * time.Millisecond // 1s debounce + safety margin

func setupDB(t *testing.T) (*pgxpool.Pool, context.Context, context.CancelFunc) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	// Run migrations from a clean state. The compose pg uses tmpfs so this
	// is a fresh DB on every `up`, but we still drop+create the public
	// schema to guarantee idempotence across test runs in one session.
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	_ = m.Drop() // tolerate "no schema" on first run
	src2, _ := iofs.New(pgmigrations.FS, ".")
	m2, err := migrate.NewWithSourceInstance("iofs", src2, dsn)
	if err != nil {
		t.Fatalf("migrate re-init: %v", err)
	}
	if err := m2.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		cancel()
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
	})
	return pool, ctx, cancel
}

// TestIntegration_BootstrapEmptyAndAutoSeed covers Bootstrap over a
// completely empty DB (no AutoSeedDir → snapshot stays empty).
func TestIntegration_BootstrapEmpty(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	cat, listener, stores, err := Bootstrap(ctx, BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	_ = listener
	if got := len(cat.Current().providersByID); got != 0 {
		t.Errorf("empty bootstrap: providers=%d, want 0", got)
	}
	if got := len(cat.Current().modelsByID); got != 0 {
		t.Errorf("empty bootstrap: models=%d, want 0", got)
	}
	_ = stores
}

// TestIntegration_NotifyPropagatesUpsert is the headline test: a direct
// write via stores.Provider.Upsert must reach the snapshot within ~1.5s
// (1s debounce + slack) through the LISTEN/NOTIFY path.
func TestIntegration_NotifyPropagatesUpsert(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	cat, listener, stores, err := Bootstrap(ctx, BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	listenerCtx, listenerCancel := context.WithCancel(ctx)
	defer listenerCancel()
	go func() { _ = listener.Run(listenerCtx) }()
	time.Sleep(200 * time.Millisecond) // let LISTEN attach

	p := &provider.Provider{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "openai-test",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
	}
	if err := stores.Provider.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	time.Sleep(flushPad)

	if _, ok := cat.Current().Provider(p.Meta.ID); !ok {
		t.Fatalf("provider %s not in snapshot after NOTIFY+debounce", p.Meta.ID)
	}
}

// TestIntegration_DeleteCascadesToPricing exercises the reverse-ref
// cascade end-to-end. A Pricing targets a Model; we delete the Model and
// expect the Pricing to be evicted on the same NOTIFY round.
func TestIntegration_DeleteCascadesToPricing(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	cat, listener, stores, err := Bootstrap(ctx, BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	listenerCtx, listenerCancel := context.WithCancel(ctx)
	defer listenerCancel()
	go func() { _ = listener.Run(listenerCtx) }()
	time.Sleep(200 * time.Millisecond)

	// Build a coherent set: provider → host → model → pricing.
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-x", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-x-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.openai.com"},
	}
	mdl := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-test", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{
			Hosts: []model.HostBinding{{HostID: hst.Meta.ID, Adapter: adapters.OpenAI}},
		},
	}
	pr := &pricing.Pricing{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pr-test", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{mdl.Meta.ID},
			Rates:          []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1}},
		},
	}
	for _, op := range []func() error{
		func() error { return stores.Provider.Upsert(ctx, prov) },
		func() error { return stores.Host.Upsert(ctx, hst) },
		func() error { return stores.Model.Upsert(ctx, mdl) },
		func() error { return stores.Pricing.Upsert(ctx, pr) },
	} {
		if err := op(); err != nil {
			t.Fatalf("seed write: %v", err)
		}
	}
	time.Sleep(flushPad)

	s := cat.Current()
	if _, ok := s.Pricing(pr.Meta.ID); !ok {
		t.Fatalf("pricing %s not in snapshot pre-delete", pr.Meta.ID)
	}
	if _, ok := s.PriceByModelHost(mdl.Meta.ID, hst.Meta.ID); !ok {
		t.Fatalf("pricingByModelHost lookup failed pre-delete")
	}

	// Disable the model (the common operational toggle; hard-delete is
	// blocked by FK RESTRICT until admin clears dependents). The Pricing's
	// TargetModelID becomes dangling → reverse-ref cascade should evict
	// the Pricing from snapshot too.
	fls := false
	mdl.Spec.Enabled = &fls
	if err := stores.Model.Upsert(ctx, mdl); err != nil {
		t.Fatalf("disable model: %v", err)
	}
	time.Sleep(flushPad)

	s = cat.Current()
	if _, ok := s.Model(mdl.Meta.ID); ok {
		t.Fatalf("model %s still present after disable", mdl.Meta.ID)
	}
	if _, ok := s.Pricing(pr.Meta.ID); ok {
		t.Fatalf("pricing %s not cascaded out after model disable", pr.Meta.ID)
	}
}

// TestIntegration_HostKeyStoredMode exercises the encrypt/decrypt path
// in hostkey.Store — that path can't be unit-tested without a master key
// and a real DB column round-trip.
func TestIntegration_HostKeyStoredMode(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	masterKey := []byte(strings.Repeat("k", 32))
	_, listener, stores, err := Bootstrap(ctx, BootstrapOptions{Pool: pool, MasterKey: masterKey})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	listenerCtx, listenerCancel := context.WithCancel(ctx)
	defer listenerCancel()
	go func() { _ = listener.Run(listenerCtx) }()

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-stored", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.openai.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	tier := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-stored-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}},
	}
	if err := stores.Policy.Upsert(ctx, tier); err != nil {
		t.Fatalf("upsert tier policy: %v", err)
	}

	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "stored-k", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{
			HostID:    hst.Meta.ID,
			PolicyID:  tier.Meta.ID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored},
			Value:     "sk-test-secret",
		},
	}
	if err := stores.HostKey.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert hostkey: %v", err)
	}
	got, err := stores.HostKey.Get(ctx, k.Meta.ID)
	if err != nil {
		t.Fatalf("get hostkey: %v", err)
	}
	if got.Resolved != "sk-test-secret" {
		t.Errorf("hostkey resolved value mismatch: got %q", got.Resolved)
	}

	if v := stores.HostKey.KeyVersion(); v != 1 {
		t.Errorf("expected key version 1 pre-rotate, got %d", v)
	}
	newKey := []byte(strings.Repeat("n", 32))
	res, err := stores.HostKey.Rotate(ctx, newKey)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if res.Rotated != 1 || res.NewVersion != 2 {
		t.Errorf("rotate result: got %+v want {1, 2}", res)
	}
	if v := stores.HostKey.KeyVersion(); v != 2 {
		t.Errorf("expected key version 2 post-rotate, got %d", v)
	}

	// After rotation the in-process key is the new one — Get must still
	// resolve the same cleartext.
	got2, err := stores.HostKey.Get(ctx, k.Meta.ID)
	if err != nil {
		t.Fatalf("get hostkey post-rotate: %v", err)
	}
	if got2.Resolved != "sk-test-secret" {
		t.Errorf("post-rotate resolved mismatch: got %q", got2.Resolved)
	}

	// Ciphertext must have changed (re-encrypted under new key with fresh
	// nonce). Pull raw rows to verify.
	rows, err := gen.New(pool).ListStoredSecretsForRotation(ctx)
	if err != nil {
		t.Fatalf("list rotated: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 stored row, got %d", len(rows))
	}
	if !rows[0].ValueKeyVersion.Valid || rows[0].ValueKeyVersion.Int32 != 2 {
		t.Errorf("row version: got %+v want 2", rows[0].ValueKeyVersion)
	}
}
