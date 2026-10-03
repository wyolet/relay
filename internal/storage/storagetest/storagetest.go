//go:build integration

// Package storagetest gives every integration test its own Postgres database
// on the server named by RELAY_TEST_PG_DSN, so test packages run in parallel
// without sharing rows, schema versions, or NOTIFY channels.
//
// A migrated template database is built once per schema (its name carries
// the newest migration version and a hash of the migration files) and each
// DB call clones it with CREATE DATABASE ... TEMPLATE. Databases are dropped
// on test cleanup; a killed run leaves them behind for `docker compose down -v`.
// Tests skip when RELAY_TEST_PG_DSN is unset.
package storagetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

// EnvDSN names the server (and credentials) the test databases live on.
const EnvDSN = "RELAY_TEST_PG_DSN"

// templateLockKey serializes template builds across concurrent test binaries.
const templateLockKey = "relay_test_template"

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// DB returns the DSN of a fresh database at the newest schema version.
func DB(t testing.TB) string {
	t.Helper()
	server := serverDSN(t)
	templateOnce.Do(func() { templateName, templateErr = ensureTemplate(server) })
	if templateErr != nil {
		t.Fatalf("storagetest: template database: %v", templateErr)
	}
	return createDB(t, server, templateName)
}

// EmptyDB returns the DSN of a fresh database no migration has touched, for
// tests that drive the schema version themselves.
func EmptyDB(t testing.TB) string {
	t.Helper()
	return createDB(t, serverDSN(t), "")
}

// Pool opens a pool on a fresh migrated database, closed on cleanup.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), DB(t))
	if err != nil {
		t.Fatalf("storagetest: open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func serverDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skip(EnvDSN + " not set; run via `make test-integration`")
	}
	return dsn
}

func createDB(t testing.TB, server, template string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := "relay_t_" + packageSlug() + "_" + randomHex(6)
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		stmt += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	if err := execOnServer(ctx, server, stmt); err != nil {
		t.Fatalf("storagetest: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := execOnServer(ctx, server, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("storagetest: %v", err)
		}
	})
	dsn, err := withDatabase(server, name)
	if err != nil {
		t.Fatalf("storagetest: %v", err)
	}
	return dsn
}

// ensureTemplate builds the template under a server-wide advisory lock,
// migrating a scratch database and renaming it into place so a crashed
// build never leaves a half-migrated template behind.
func ensureTemplate(server string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	version, digest, err := migrationsVersion()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("relay_test_template_%d_%s", version, digest)

	conn, err := pgx.Connect(ctx, server)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", templateLockKey); err != nil {
		return "", fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", templateLockKey) //nolint:errcheck

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look up template: %w", err)
	}
	if exists {
		return name, nil
	}

	building := name + "_build"
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{building}.Sanitize()+" WITH (FORCE)"); err != nil {
		return "", fmt.Errorf("drop stale build: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{building}.Sanitize()); err != nil {
		return "", fmt.Errorf("create build: %w", err)
	}
	buildDSN, err := withDatabase(server, building)
	if err != nil {
		return "", err
	}
	if err := storage.MigrateTo(buildDSN, version); err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{building}.Sanitize()+" RENAME TO "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("rename build: %w", err)
	}
	return name, nil
}

// LatestVersion is the newest migration version embedded in the binary.
func LatestVersion(t testing.TB) uint {
	t.Helper()
	v, _, err := migrationsVersion()
	if err != nil {
		t.Fatalf("storagetest: %v", err)
	}
	return v
}

func migrationsVersion() (uint, string, error) {
	names, err := fs.Glob(pgmigrations.FS, "*.sql")
	if err != nil {
		return 0, "", err
	}
	sort.Strings(names)
	h := sha256.New()
	var latest uint
	for _, n := range names {
		body, err := fs.ReadFile(pgmigrations.FS, n)
		if err != nil {
			return 0, "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(body))
		h.Write(body)
		prefix, _, _ := strings.Cut(n, "_")
		v, err := strconv.ParseUint(prefix, 10, 32)
		if err != nil {
			return 0, "", fmt.Errorf("migration %s: version prefix: %w", n, err)
		}
		latest = max(latest, uint(v))
	}
	return latest, hex.EncodeToString(h.Sum(nil))[:8], nil
}

// execOnServer runs a database-level statement on the DSN's own database.
// CREATE DATABASE ... TEMPLATE fails while anything else touches the
// template; the only such toucher is a concurrent clone, so retry briefly.
func execOnServer(ctx context.Context, server, stmt string) error {
	conn, err := pgx.Connect(ctx, server)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	for attempt := 0; ; attempt++ {
		_, err = conn.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if err == nil || attempt == 50 || !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	return nil
}

// withDatabase points a URL or keyword/value DSN at another database.
func withDatabase(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", EnvDSN, err)
		}
		u.Path = "/" + name
		return u.String(), nil
	}
	return dsn + " dbname=" + name, nil
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// packageSlug names the test binary's package (go test runs in its
// directory) so a leaked database says where it came from.
func packageSlug() string {
	wd, err := os.Getwd()
	if err != nil {
		return "pkg"
	}
	s := strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(filepath.Base(wd)), "_"), "_")
	if len(s) > 24 {
		s = s[:24]
	}
	if s == "" {
		s = "pkg"
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
