//go:build integration

// Live ClickHouse schema-upgrade check. Runs only with -tags=integration AND
// RELAY_TEST_CH_DSN set (else skipped).
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/usage/clickhouse/ -run Integration -v

package clickhouse

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/usage"
)

// A table without the attribution columns must be upgraded in place by
// ensureSchema, not rejected. ensureSchema addresses usage_events unqualified,
// so the fixture gets a throwaway database instead of RELAY_TEST_CH_DSN's own.
func TestIntegration_EnsureSchemaAddsAttributionColumns(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse schema check")
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
	// Registered before CREATE, and owning admin's Close: a deferred Close
	// would run before any Cleanup and leave the DROP a closed connection.
	t.Cleanup(func() {
		defer admin.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+db); err != nil {
			t.Errorf("cleanup: drop database %s: %v", db, err)
		}
	})
	if err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}

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
		"credential_kind", "credential_id", "relay_key",
	} {
		if !have[c] {
			t.Fatalf("column %q missing after ensureSchema", c)
		}
	}
}

// Every inserted column reads back as the value insertValues gives, across
// two batches through one reused column set.
func TestIntegration_InsertedRowsMatchInsertValues(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse insert check")
	}
	s, err := New(Config{DSN: scratchDatabase(t, dsn), WALDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if opts, err := clickhouse.ParseDSN(dsn); err == nil && opts.Protocol == clickhouse.Native && s.native == nil {
		t.Fatal("native DSN did not get the column-reusing inserter")
	}
	ctx := context.Background()

	events := sampleEvents()
	for _, batch := range [][]usage.Event{events[:3], events[3:]} {
		if err := s.insertBatch(batch); err != nil {
			t.Fatalf("insertBatch: %v", err)
		}
	}

	byID := map[string]usage.Event{}
	for _, ev := range events {
		byID[ev.RequestID] = ev
	}
	rows, err := s.conn.Query(ctx, "SELECT "+strings.Join(insertColumns, ", ")+" FROM "+chTable)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		ref := insertValues(usage.Event{})
		dest := make([]any, len(ref))
		for i, v := range ref {
			dest[i] = reflect.New(reflect.TypeOf(v)).Interface()
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		id := *dest[0].(*string)
		ev, ok := byID[id]
		if !ok {
			t.Fatalf("unexpected row %q", id)
		}
		seen++
		want := wantRow(ev)
		for i, d := range dest {
			got := reflect.ValueOf(d).Elem().Interface()
			if ts, ok := got.(time.Time); ok {
				got = proto.ToDateTime64(ts, proto.PrecisionNano)
			}
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("row %q column %s: got %#v, want %#v", id, insertColumns[i], got, want[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != len(events) {
		t.Fatalf("read back %d rows, want %d", seen, len(events))
	}
}

// A connection that died between batches is replaced before the insert.
func TestIntegration_InsertRedialsClosedConnection(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse insert check")
	}
	s, err := New(Config{DSN: scratchDatabase(t, dsn), WALDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.native == nil {
		t.Skip("HTTP DSN inserts through clickhouse-go")
	}
	events := sampleEvents()
	if err := s.insertBatch(events[:1]); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_ = s.native.client.Close()
	if err := s.insertBatch(events[1:2]); err != nil {
		t.Fatalf("insert after the connection closed: %v", err)
	}
	var n uint64
	if err := s.conn.QueryRow(context.Background(), "SELECT count() FROM "+chTable).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count: %d, %v", n, err)
	}
}

// A writer one version behind names only the columns it knows, so a column a
// newer relay added (with a default, anywhere in the table) must not fail its
// batch.
func TestIntegration_InsertIntoTableWithNewerColumn(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse insert check")
	}
	s, err := New(Config{DSN: scratchDatabase(t, dsn), WALDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.conn.Exec(ctx,
		"ALTER TABLE "+chTable+" ADD COLUMN newer_col LowCardinality(String) DEFAULT 'unset' AFTER source"); err != nil {
		t.Fatalf("add newer column: %v", err)
	}

	ev := usage.Event{
		RequestID: "req-newer-col", Source: "pipeline", Timestamp: time.Now().UTC(),
		Status: 200, ModelID: "m1", PolicyID: "p1", RelayKey: "k1",
	}
	if err := s.insertBatch([]usage.Event{ev}); err != nil {
		t.Fatalf("insertBatch into a table with a newer column: %v", err)
	}

	var newer, relayKey string
	if err := s.conn.QueryRow(ctx,
		"SELECT newer_col, relay_key FROM "+chTable+" WHERE request_id = ?", ev.RequestID).Scan(&newer, &relayKey); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if newer != "unset" || relayKey != "k1" {
		t.Fatalf("got newer_col=%q relay_key=%q, want \"unset\" and \"k1\"", newer, relayKey)
	}
}
