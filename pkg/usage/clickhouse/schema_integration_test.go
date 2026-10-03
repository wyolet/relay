//go:build integration

// Live ClickHouse schema-upgrade check. Runs only with -tags=integration AND
// RELAY_CH_DSN set (else skipped).
//
//	RELAY_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/usage/clickhouse/ -run Integration -v
package clickhouse

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// A table without the attribution columns must be upgraded in place by
// ensureSchema, not rejected. ensureSchema addresses usage_events unqualified,
// so the fixture gets a throwaway database instead of RELAY_CH_DSN's own.
func TestIntegration_EnsureSchemaAddsAttributionColumns(t *testing.T) {
	dsn := os.Getenv("RELAY_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_CH_DSN unset; skipping live ClickHouse schema check")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctx := context.Background()
	db := fmt.Sprintf("relay_schema_test_%d", time.Now().UnixNano())

	admin, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()
	if err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db); err != nil {
			t.Logf("cleanup: drop database %s: %v", db, err)
		}
	})

	scoped := *opts
	scoped.Auth.Database = db
	conn, err := clickhouse.Open(&scoped)
	if err != nil {
		t.Fatalf("open %s: %v", db, err)
	}
	defer conn.Close()

	// The pre-attribution schema is this file's DDL truncated at the first
	// new column, so the fixture can't drift from the current one.
	withoutAttribution := strings.SplitN(createTableSQL, "    project_id", 2)[0]
	withoutAttribution = strings.TrimSuffix(strings.TrimRight(withoutAttribution, " \n"), ",") + `
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, model_id, policy_id)`
	if err := conn.Exec(ctx, withoutAttribution); err != nil {
		t.Fatalf("create table without attribution columns: %v", err)
	}

	if err := ensureSchema(ctx, conn, 30); err != nil {
		t.Fatalf("ensureSchema on a pre-attribution table: %v", err)
	}

	rows, err := conn.Query(ctx,
		"SELECT name FROM system.columns WHERE database = ? AND table = ?", db, chTable)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		have[name] = true
	}
	for _, c := range []string{
		"project_id", "project", "team_id", "team",
		"principal_kind", "principal_id", "principal",
		"credential_kind", "credential_id",
	} {
		if !have[c] {
			t.Fatalf("column %q missing after ensureSchema", c)
		}
	}
}
