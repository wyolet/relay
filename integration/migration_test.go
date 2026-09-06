//go:build integration

// migration_test.go covers the schema-lifecycle promises the rollback story
// rests on: the key backfill has to give every legacy key its own service
// account however long its name is, a down-migration has to land on the
// version it was asked for and converge when the schema is brought back up,
// a target above the current version is a mistake rather than an
// up-migration, and a pod that boots with auto-migration switched off has to
// leave the schema where the operator put it.
//
// Every test here runs against a database of its own: down-migrations strip
// tables the rest of the suite shares.
package integration_test

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	storagemod "github.com/wyolet/relay/internal/storage"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
	"github.com/wyolet/relay/pkg/ids"
)

// scratchPrefix marks every database these tests create, so a run that dies
// before its cleanup can be swept up by the next one.
const scratchPrefix = "relay_scratch_"

// scratchDB creates a database of its own next to the one the suite runs
// against and returns its DSN. Dropped on cleanup.
func scratchDB(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("RELAY_TEST_PG_DSN")
	if base == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; skipping integration test")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	suffix := strings.ReplaceAll(ids.New(), "-", "")
	db := scratchPrefix + name + "_" + suffix[len(suffix)-12:]

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	// A run killed mid-test never gets its cleanup, so sweep what earlier
	// runs left behind before adding one more.
	rows, err := admin.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1`, scratchPrefix+"%")
	if err != nil {
		t.Fatalf("list scratch databases: %v", err)
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan scratch database: %v", err)
		}
		stale = append(stale, name)
	}
	rows.Close()
	for _, name := range stale {
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgIdent(name)+` WITH (FORCE)`); err != nil {
			t.Logf("drop leftover database %s: %v", name, err)
		}
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgIdent(db)); err != nil {
		t.Fatalf("create database %s: %v", db, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, base)
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.Exec(ctx, `DROP DATABASE IF EXISTS `+pgIdent(db)+` WITH (FORCE)`)
	})

	u.Path = "/" + db
	return u.String()
}

func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// headVersion is the newest migration in the embedded set, read rather than
// pinned so adding a migration does not need this file edited.
func headVersion(t *testing.T) uint {
	t.Helper()
	entries, err := pgmigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var head uint
	for _, e := range entries {
		n, err := strconv.ParseUint(strings.SplitN(e.Name(), "_", 2)[0], 10, 64)
		if err != nil {
			continue
		}
		if uint(n) > head {
			head = uint(n)
		}
	}
	if head == 0 {
		t.Fatal("no numbered migrations found")
	}
	return head
}

// scratchMigrator drives the embedded migrations against a scratch DSN.
// MigrateTo is the rollback entry point: it refuses any target above the
// schema's current version, so stepping a fixture from one version to the
// next needs the migrator itself.
func scratchMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

// schemaVersion reads the migrate bookkeeping row.
func schemaVersion(t *testing.T, dsn string) (version uint, dirty bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var v int64
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return uint(v), dirty
}

// insertLegacyKey writes a relay_keys row from before the principal columns
// existed: an owner naming a user that carries no id.
func insertLegacyKey(t *testing.T, dsn, name string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
		 VALUES ($1, $2, '', $3, '{"owner":{"kind":"user"}}'::jsonb, '{}'::jsonb)`,
		ids.New(), name, sha256Hex(name)); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

// Every legacy key gets a service account of its own, whatever its name:
// two keys whose names share a long prefix must not land on one principal,
// which would let either key spend the other's grants.
func TestMigrationGivesLongLegacyKeyNamesDistinctServiceAccounts(t *testing.T) {
	dsn := scratchDB(t, "relay_mig_names")
	m := scratchMigrator(t, dsn)
	if err := m.Migrate(25); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 25: %v", err)
	}

	prefix := strings.Repeat("a", 66)
	first, second := prefix+"one", prefix+"two"
	insertLegacyKey(t, dsn, first)
	insertLegacyKey(t, dsn, second)

	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate to 26: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var accounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 2 {
		t.Errorf("service accounts = %d, want one per legacy key", accounts)
	}

	var principals int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT principal_sa_id) FROM relay_keys WHERE principal_sa_id IS NOT NULL`).Scan(&principals); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	if principals != 2 {
		t.Errorf("distinct key principals = %d, want the two keys on separate accounts", principals)
	}

	var maxLen int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(length(name)), 0) FROM service_accounts`).Scan(&maxLen); err != nil {
		t.Fatalf("read account names: %v", err)
	}
	if maxLen > 63 {
		t.Errorf("generated account name is %d chars, past the DNS-1123 label limit", maxLen)
	}
}

// `migrate down` has to land on exactly the version asked for, and bringing
// the schema back up has to reach head from there.
func TestMigrateDownToATargetThenUpReachesHead(t *testing.T) {
	dsn := scratchDB(t, "relay_mig_cycle")
	head := headVersion(t)
	if head <= 25 {
		t.Fatalf("head is %d; this test needs migrations above 25", head)
	}

	st, err := storagemod.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()
	if v, dirty := schemaVersion(t, dsn); v != head || dirty {
		t.Fatalf("after boot the schema is at %d (dirty=%v), want head %d", v, dirty, head)
	}

	if err := storagemod.MigrateTo(dsn, 25); err != nil {
		t.Fatalf("migrate down to 25: %v", err)
	}
	if v, dirty := schemaVersion(t, dsn); v != 25 || dirty {
		t.Fatalf("after the down migration the schema is at %d (dirty=%v), want 25", v, dirty)
	}

	st, err = storagemod.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st.Close()
	if v, dirty := schemaVersion(t, dsn); v != head || dirty {
		t.Errorf("after coming back up the schema is at %d (dirty=%v), want head %d", v, dirty, head)
	}
}

// A target above the current version would run the up-migrations the
// operator is trying to undo; it is refused and the schema is left alone.
func TestMigrateDownRefusesATargetAboveTheSchemaVersion(t *testing.T) {
	dsn := scratchDB(t, "relay_mig_target")
	head := headVersion(t)
	if err := storagemod.MigrateTo(dsn, 25); err != nil {
		t.Fatalf("migrate to 25: %v", err)
	}

	err := storagemod.MigrateTo(dsn, head)
	if err == nil {
		t.Fatal("a target above the current version was accepted")
	}
	if !strings.Contains(err.Error(), "the schema is at 25") {
		t.Errorf("the error does not name the current version: %v", err)
	}
	if v, _ := schemaVersion(t, dsn); v != 25 {
		t.Errorf("the refused call moved the schema to %d, want it left at 25", v)
	}
}

// A pod restarting mid-rollback must not re-apply the migrations the
// operator just unwound.
func TestBootWithMigrationsOffLeavesTheSchemaVersion(t *testing.T) {
	dsn := scratchDB(t, "relay_mig_boot")
	head := headVersion(t)
	if err := storagemod.MigrateTo(dsn, 25); err != nil {
		t.Fatalf("migrate to 25: %v", err)
	}

	st, err := storagemod.Open(context.Background(), dsn, storagemod.WithMigrateOnBoot(false))
	if err != nil {
		t.Fatalf("open with migrations off: %v", err)
	}
	if err := st.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	st.Close()
	if v, _ := schemaVersion(t, dsn); v != 25 {
		t.Errorf("a boot with migrations off moved the schema to %d, want it left at 25", v)
	}

	// The default is still to migrate, so a pod on the new image catches up.
	st, err = storagemod.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open with the default: %v", err)
	}
	st.Close()
	if v, _ := schemaVersion(t, dsn); v != head {
		t.Errorf("the default boot left the schema at %d, want head %d", v, head)
	}
}
