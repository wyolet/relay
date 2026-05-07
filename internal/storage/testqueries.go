//go:build integration

package storage

// testqueries.go exposes typed query helpers for integration tests.
// It also provides test-scoped constructors that register t.Cleanup automatically.
// All raw SQL lives here (excluded from the no-SQL-outside-storage grep rule).
// Building only under the "integration" tag keeps this out of production binaries.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MustOpenPool opens a pgxpool.Pool and registers t.Cleanup to close it.
func MustOpenPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("MustOpenPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SeedMinimalCatalog inserts the minimal valid catalog rows needed by tests.
func SeedMinimalCatalog(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
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
	return err
}

// SeedProviderRow inserts a single provider row for test setup.
func SeedProviderRow(ctx context.Context, pool *pgxpool.Pool, name, metadata, spec string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO providers (name, metadata, spec) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
		name, metadata, spec)
	return err
}

// SeedProviderRow2 inserts a second provider (used by Reload tests).
func SeedProviderRow2(ctx context.Context, pool *pgxpool.Pool, name, metadata, spec string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO providers (name, metadata, spec) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
		name, metadata, spec)
	return err
}

// SeedMalformedProvider inserts a provider row with an invalid spec JSON for error-path tests.
func SeedMalformedProvider(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO providers (name, metadata, spec) VALUES ($1, $2, $3)",
		"bad", `{"Name":"bad"}`, `{"kind":12345,"baseURL":"http://localhost"}`)
	return err
}

// SeedLegacySecretRow inserts a pre-migration-000002 secret row (no value_kind columns).
func SeedLegacySecretRow(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO secrets (name, metadata, spec) VALUES ($1, $2, $3)",
		"legacy-env",
		`{"Name":"legacy-env","Labels":{}}`,
		`{"provider":"ollama","valueFrom":{"env":"LEGACY_TEST_VAR"}}`)
	return err
}

// SeedStoredSecret inserts a stored-mode secret directly (bypassing the PGStore encryption layer).
// Used to test error paths where the master key is absent or tampered.
func SeedStoredSecret(ctx context.Context, pool *pgxpool.Pool, name string, ciphertext, nonce []byte) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO secrets (name, metadata, spec, value_kind, value_ciphertext, value_nonce) VALUES ($1, $2, $3, 'stored', $4, $5)",
		name, `{"Name":"`+name+`"}`, "{}", ciphertext, nonce)
	return err
}

// SeedBadConstraintSecret inserts a secret row that violates the check constraint (env + ciphertext).
// Returns the pg error; the caller asserts it is non-nil.
func SeedBadConstraintSecret(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO secrets (name, metadata, spec, value_kind, value_from_env, value_ciphertext, value_nonce) VALUES ($1, $2, $3, 'env', 'SOMEVAR', $4, $5)",
		"bad", `{"Name":"bad"}`, "{}",
		[]byte{0xde, 0xad, 0xbe, 0xef},
		make([]byte, 12))
	return err
}

// QuerySecretBackfill reads value_kind and value_from_env for a given secret name (migration test).
func QuerySecretBackfill(ctx context.Context, pool *pgxpool.Pool, name string) (valueKind, valueFromEnv string, err error) {
	err = pool.QueryRow(ctx,
		"SELECT value_kind, value_from_env FROM secrets WHERE name = $1", name).
		Scan(&valueKind, &valueFromEnv)
	return
}

// QuerySecretCiphertext reads value_ciphertext and value_nonce for a given secret name.
func QuerySecretCiphertext(ctx context.Context, pool *pgxpool.Pool, name string) (ct, nonce []byte, err error) {
	err = pool.QueryRow(ctx,
		"SELECT value_ciphertext, value_nonce FROM secrets WHERE name = $1", name).
		Scan(&ct, &nonce)
	return
}

// QuerySecretEnvRow reads value_kind, value_from_env, value_ciphertext for a given secret name.
func QuerySecretEnvRow(ctx context.Context, pool *pgxpool.Pool, name string) (valueKind, valueFromEnv string, ct []byte, err error) {
	err = pool.QueryRow(ctx,
		"SELECT value_kind, value_from_env, value_ciphertext FROM secrets WHERE name = $1", name).
		Scan(&valueKind, &valueFromEnv, &ct)
	return
}

// CountSecrets returns the row count for a given secret name.
func CountSecrets(ctx context.Context, pool *pgxpool.Pool, name string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM secrets WHERE name = $1", name).Scan(&n)
	return n, err
}

// CountProviders returns the total number of provider rows.
func CountProviders(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM providers").Scan(&n)
	return n, err
}

// CurrentTxID returns the current transaction ID (used by seed isolation tests).
func CurrentTxID(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, "SELECT txid_current()").Scan(&id)
	return id, err
}

// QuerySecretStoredRow reads value_ciphertext and value_from_env for ciphertext verification.
// envNull is nil when value_from_env IS NULL (expected for stored-mode).
func QuerySecretStoredRow(ctx context.Context, pool *pgxpool.Pool, name string) (ct []byte, envNull *string, err error) {
	err = pool.QueryRow(ctx,
		"SELECT value_ciphertext, value_from_env FROM secrets WHERE name = $1", name).
		Scan(&ct, &envNull)
	return
}

// QuerySecretStoredCiphertext reads only value_ciphertext for a given secret name.
func QuerySecretStoredCiphertext(ctx context.Context, pool *pgxpool.Pool, name string) ([]byte, error) {
	var ct []byte
	err := pool.QueryRow(ctx, "SELECT value_ciphertext FROM secrets WHERE name = $1", name).Scan(&ct)
	return ct, err
}
