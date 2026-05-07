//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	pgmigrations "github.com/wyolet/relay/migrations/postgres"

	storagemod "github.com/wyolet/relay/internal/storage"
)

func startPG(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("relay_test"),
		tcpostgres.WithUsername("relay"),
		tcpostgres.WithPassword("relay"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start pg: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	return dsn
}

func runMigrationsForTest(t *testing.T, dsn string) {
	t.Helper()
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs: %v", err)
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

// minimalYAML writes a valid minimal YAML fixture to a temp dir and returns the dir path.
func minimalYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	content := `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: ollama-test
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: llama3-test
spec:
  provider: ollama-test
  upstreamName: llama3:8b
---
apiVersion: relay.wyolet.dev/v1
kind: Route
metadata:
  name: default-test
spec:
  default: true
  models:
    - llama3-test
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return dir
}

func TestSeed_DryRun(t *testing.T) {
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)
	dir := minimalYAML(t)

	var buf bytes.Buffer
	if err := runSeedTo(&buf, []string{"--from", dir, "--dsn", dsn}); err != nil {
		t.Fatalf("dry-run error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "dry-run") {
		t.Errorf("expected dry-run notice, got: %s", out)
	}
	if !strings.Contains(out, "ollama-test") {
		t.Errorf("expected provider in diff output, got: %s", out)
	}

	// Verify no rows were written.
	ctx := context.Background()
	pool := storagemod.MustOpenPool(ctx, t, dsn)
	n, err := storagemod.CountProviders(ctx, pool)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("dry-run wrote rows: got %d providers, want 0", n)
	}
}

func TestSeed_Apply_And_Idempotent(t *testing.T) {
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)
	dir := minimalYAML(t)

	// First apply.
	var buf bytes.Buffer
	if err := runSeedTo(&buf, []string{"--from", dir, "--dsn", dsn, "--apply"}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !strings.Contains(buf.String(), "applied") {
		t.Errorf("expected 'applied' in output, got: %s", buf.String())
	}

	ctx := context.Background()
	pool := storagemod.MustOpenPool(ctx, t, dsn)
	n1, err := storagemod.CountProviders(ctx, pool)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n1 == 0 {
		t.Fatal("expected rows after --apply, got 0")
	}

	// Second apply (no-op).
	buf.Reset()
	if err := runSeedTo(&buf, []string{"--from", dir, "--dsn", dsn, "--apply"}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !strings.Contains(buf.String(), "no changes") {
		t.Logf("second apply output: %s", buf.String())
	}

	n2, err := storagemod.CountProviders(ctx, pool)
	if err != nil {
		t.Fatalf("count2: %v", err)
	}
	if n1 != n2 {
		t.Errorf("row count changed after second apply: %d -> %d", n1, n2)
	}
}

func TestSeed_BrokenYAML(t *testing.T) {
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)

	dir := t.TempDir()
	// "\t\t: invalid" is unparseable YAML (tab indentation error).
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("\t\t: invalid"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var buf bytes.Buffer
	err := runSeedTo(&buf, []string{"--from", dir, "--dsn", dsn, "--apply"})
	if err == nil {
		t.Fatal("expected error from broken YAML, got nil")
	}

	// No transaction should have been opened — verify providers empty.
	ctx2 := context.Background()
	pool2 := storagemod.MustOpenPool(ctx2, t, dsn)
	n, err2 := storagemod.CountProviders(ctx2, pool2)
	if err2 != nil {
		t.Fatalf("count: %v", err2)
	}
	if n != 0 {
		t.Errorf("broken yaml wrote rows: got %d", n)
	}
}

func TestAutoSeed_FirstBoot_ThenNoop(t *testing.T) {
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)
	dir := minimalYAML(t)

	t.Setenv("RELAY_CONFIG_DIR", dir)

	ctx := context.Background()

	// First boot: empty DB → seeds.
	if err := maybeAutoSeed(ctx, dsn); err != nil {
		t.Fatalf("first auto-seed: %v", err)
	}

	pool := storagemod.MustOpenPool(ctx, t, dsn)
	n1, err := storagemod.CountProviders(ctx, pool)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n1 == 0 {
		t.Fatal("expected rows after auto-seed, got 0")
	}

	// Second boot: non-empty → no-op.
	if err := maybeAutoSeed(ctx, dsn); err != nil {
		t.Fatalf("second auto-seed: %v", err)
	}

	n2, err := storagemod.CountProviders(ctx, pool)
	if err != nil {
		t.Fatalf("count2: %v", err)
	}
	if n1 != n2 {
		t.Errorf("auto-seed noop changed row count: %d -> %d", n1, n2)
	}
}
