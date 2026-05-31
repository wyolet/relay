package jobq

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.up.sql
var migrationsFS embed.FS

// Migrate applies jobq's pending schema migrations to the database behind pool.
// It is idempotent and safe to call at every boot. Applied versions are
// tracked in jobq_schema_migrations — jobq's own lineage, independent of any
// other migrator using the same database.
//
// Each migration runs in its own transaction together with its bookkeeping
// insert, so a crash leaves the database at a clean version boundary.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS jobq_schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("jobq: create migrations table: %w", err)
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("jobq: read migrations: %w", err)
	}
	var versions []string
	files := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version := strings.TrimSuffix(name, ".up.sql")
		versions = append(versions, version)
		files[version] = name
	}
	sort.Strings(versions)

	for _, v := range versions {
		if applied[v] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + files[v])
		if err != nil {
			return fmt.Errorf("jobq: read migration %s: %w", v, err)
		}
		if err := applyOne(ctx, pool, v, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM jobq_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("jobq: load applied migrations: %w", err)
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, version, body string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("jobq: begin migration %s: %w", version, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("jobq: apply migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO jobq_schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("jobq: record migration %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("jobq: commit migration %s: %w", version, err)
	}
	return nil
}
