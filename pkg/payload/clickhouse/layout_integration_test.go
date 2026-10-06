//go:build integration

package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/wyolet/relay/pkg/payload"
)

// pieceHex is the stored hash of one array element, as lower-case hex.
func pieceHex(element string) string {
	sum := sha256.Sum256([]byte(element))
	return hex.EncodeToString(sum[:16])
}

func countPiece(t *testing.T, conn clickhouse.Conn, project, element string) uint64 {
	t.Helper()
	return countRows(t, conn, "SELECT count() FROM payload_pieces FINAL WHERE project_id = ? AND lower(hex(hash)) = ?",
		project, pieceHex(element))
}

// syncMutations makes an ALTER ... UPDATE/DELETE return only once applied.
func syncMutations(ctx context.Context) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2}))
}

func writeDedup(t *testing.T, dsn string, recs []payload.Record) {
	t.Helper()
	s, err := New(Config{DSN: dsn, WALDir: t.TempDir(), Dedup: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeAll(t, s, recs)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func openReader(t *testing.T, dsn string) *Reader {
	t.Helper()
	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	t.Cleanup(func() { _ = rdr.Close() })
	return rdr
}

func TestIntegration_PiecesStoredPerProject(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	const msg = `{"role":"user","content":"the same question from two tenants"}`
	now := time.Now().UTC().Truncate(time.Microsecond)
	recs := []payload.Record{
		{RequestID: "req-a", Timestamp: now, ProjectID: "p-1", PrincipalID: "u-1", RequestBody: []byte(`{"model":"m","messages":[` + msg + `]}`)},
		{RequestID: "req-b", Timestamp: now, ProjectID: "p-2", PrincipalID: "u-2", RequestBody: []byte(`{"model":"m","messages":[` + msg + `]}`)},
	}
	writeDedup(t, dsn, recs)

	for _, p := range []string{"p-1", "p-2"} {
		if n := countPiece(t, conn, p, msg); n != 1 {
			t.Fatalf("project %s holds %d rows of the shared message, want 1", p, n)
		}
	}
	rdr := openReader(t, dsn)
	for _, want := range recs {
		got, err := rdr.Get(context.Background(), want.RequestID)
		if err != nil {
			t.Fatalf("Get %s: %v", want.RequestID, err)
		}
		assertSameRecord(t, got, want)
	}
}

func TestIntegration_TamperedRequestFailsDigest(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	ctx := context.Background()
	want := conversationRecords(time.Now().UTC().Truncate(time.Microsecond))[3]
	writeDedup(t, dsn, []payload.Record{want})

	rdr := openReader(t, dsn)
	if got, err := rdr.Get(ctx, want.RequestID); err != nil {
		t.Fatalf("Get before tampering: %v", err)
	} else {
		assertSameRecord(t, got, want)
	}

	if err := conn.Exec(syncMutations(ctx),
		`ALTER TABLE payload_requests UPDATE skeleton = replaceOne(skeleton, '"model":"m"', '"model":"x"') WHERE request_id = ?`,
		want.RequestID); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	got, err := rdr.Get(ctx, want.RequestID)
	if !errors.Is(err, payload.ErrIntegrity) {
		t.Fatalf("Get after tampering: err = %v, want ErrIntegrity", err)
	}
	if got.RequestBody != nil || got.ResponseBody != nil {
		t.Fatalf("Get after tampering returned bodies: %+v", got)
	}
}

// The layouts an earlier release created, before per-project pieces,
// server-stamped last_seen and the request digest.
const (
	earlierPiecesSQL = `CREATE TABLE payload_pieces (
    hash FixedString(16), body String, last_seen DateTime
) ENGINE = ReplacingMergeTree(last_seen) ORDER BY hash`
	clientStampedPiecesSQL = `CREATE TABLE payload_pieces (
    project_id String, hash FixedString(16), body String, last_seen DateTime
) ENGINE = ReplacingMergeTree(last_seen) ORDER BY (project_id, hash)`
	earlierRequestsSQL = `CREATE TABLE payload_requests (
    request_id String, ts DateTime64(9, 'UTC'), project_id String, principal_id String, relay_key_hash String,
    skeleton String, field_names Array(String), field_is_array Array(UInt8), field_offsets Array(UInt32),
    field_counts Array(UInt32), hashes Array(FixedString(16)), response_body String,
    request_truncated UInt8, response_truncated UInt8
) ENGINE = MergeTree PARTITION BY toYYYYMMDD(ts) ORDER BY (ts, request_id)`
)

func columnType(t *testing.T, conn clickhouse.Conn, table, column string) string {
	t.Helper()
	cols, err := tableColumns(context.Background(), conn, table)
	if err != nil {
		t.Fatal(err)
	}
	return cols[column]
}

func hasColumn(t *testing.T, conn clickhouse.Conn, table, column string) bool {
	t.Helper()
	return columnType(t, conn, table, column) != ""
}

func TestIntegration_EmptyEarlierLayoutIsRecreated(t *testing.T) {
	for name, piecesDDL := range map[string]string{"unscoped": earlierPiecesSQL, "client-stamped": clientStampedPiecesSQL} {
		t.Run(name, func(t *testing.T) {
			_, conn := throwawayDatabase(t)
			ctx := context.Background()
			for _, ddl := range []string{piecesDDL, earlierRequestsSQL} {
				if err := conn.Exec(ctx, ddl); err != nil {
					t.Fatalf("create earlier layout: %v", err)
				}
			}
			if err := ensureSchema(ctx, conn, 7); err != nil {
				t.Fatalf("ensureSchema: %v", err)
			}
			if !hasColumn(t, conn, "payload_pieces", "project_id") || !hasColumn(t, conn, "payload_requests", "request_sha256") {
				t.Fatal("earlier layout not replaced")
			}
			if typ := columnType(t, conn, "payload_pieces", "last_seen"); typ != "DateTime64(3)" {
				t.Fatalf("payload_pieces.last_seen type = %q, want DateTime64(3)", typ)
			}
			var key, engine string
			if err := conn.QueryRow(ctx,
				"SELECT sorting_key, engine_full FROM system.tables WHERE database = currentDatabase() AND name = 'payload_pieces'").Scan(&key, &engine); err != nil {
				t.Fatal(err)
			}
			if key != "project_id, hash" {
				t.Fatalf("payload_pieces sorting key = %q, want project_id, hash", key)
			}
			if !strings.Contains(engine, "TTL toDateTime(last_seen) + toIntervalDay(8)") {
				t.Fatalf("payload_pieces engine = %q, want the last_seen TTL", engine)
			}
		})
	}
}

func TestIntegration_EarlierLayoutWithRowsIsKept(t *testing.T) {
	for _, c := range []struct {
		table, ddl, insert, column string
	}{
		{"payload_pieces", earlierPiecesSQL, "INSERT INTO payload_pieces (hash, body, last_seen) VALUES ('0123456789abcdef', 'x', now())", "project_id"},
		{"payload_requests", earlierRequestsSQL, "INSERT INTO payload_requests (request_id, ts) VALUES ('r-1', now())", "request_sha256"},
	} {
		t.Run(c.table, func(t *testing.T) {
			_, conn := throwawayDatabase(t)
			ctx := context.Background()
			if err := conn.Exec(ctx, c.ddl); err != nil {
				t.Fatalf("create earlier layout: %v", err)
			}
			if err := conn.Exec(ctx, c.insert); err != nil {
				t.Fatalf("insert: %v", err)
			}
			err := ensureSchema(ctx, conn, 0)
			if err == nil || !strings.Contains(err.Error(), c.table) {
				t.Fatalf("ensureSchema: err = %v, want an error naming %s", err, c.table)
			}
			if n := countRows(t, conn, "SELECT count() FROM "+c.table); n != 1 {
				t.Fatalf("%s rows = %d after ensureSchema, want 1", c.table, n)
			}
			if hasColumn(t, conn, c.table, c.column) {
				t.Fatalf("%s was replaced despite holding rows", c.table)
			}
		})
	}
}
