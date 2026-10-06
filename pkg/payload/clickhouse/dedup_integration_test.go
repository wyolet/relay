//go:build integration

// Live ClickHouse checks for the dedup tables. Runs only with
// -tags=integration AND RELAY_TEST_CH_DSN set (else skipped). Each test gets a
// throwaway database because ensureSchema addresses tables unqualified.
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/payload/clickhouse/ -run Integration -v

package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

// throwawayDatabase creates a database for one test and returns a DSN naming
// it plus a connection scoped to it. The database is dropped on cleanup.
func throwawayDatabase(t *testing.T) (string, clickhouse.Conn) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse check")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	db := fmt.Sprintf("relay_payload_test_%d", time.Now().UnixNano())
	u.Path = "/" + db

	admin, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Exec(context.Background(), "CREATE DATABASE "+db); err != nil {
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
	t.Cleanup(func() { _ = conn.Close() })
	return u.String(), conn
}

// conversationRecords is one growing conversation (every turn resends the
// earlier messages, the system prompt and the tools) plus a truncated body,
// a non-JSON body and a record with no request body.
func conversationRecords(start time.Time) []payload.Record {
	system := strings.Repeat("You answer briefly. ", 100)
	tools := `[{"name":"read","input_schema":{"type":"object"}},{"name":"write","input_schema":{"type":"object"}}]`
	var (
		recs     []payload.Record
		messages []string
	)
	for i := range 12 {
		messages = append(messages, fmt.Sprintf(`{"role":"user","content":"turn %d: it's a \"quoted\" \\ line"}`, i))
		recs = append(recs, payload.Record{
			RequestBody:  []byte(`{"model":"m","system":"` + system + `","messages":[` + strings.Join(messages, ",") + `],"tools":` + tools + `,"stream":true}`),
			ResponseBody: []byte(fmt.Sprintf(`{"content":[{"type":"text","text":"reply %d"}]}`, i)),
		})
	}
	last := recs[len(recs)-1].RequestBody
	recs = append(recs,
		payload.Record{RequestBody: last[:len(last)/2], RequestTruncated: true, ResponseBody: []byte("{}")},
		payload.Record{RequestBody: []byte("\x00\x01'\\binary\xff\n"), ResponseBody: []byte("\xfe")},
		payload.Record{ResponseBody: []byte(`{"only":"response"}`), ResponseTruncated: true},
	)
	for i := range recs {
		recs[i].RequestID = fmt.Sprintf("req-%02d", i)
		recs[i].Timestamp = start.Add(time.Duration(i) * time.Millisecond)
		recs[i].ProjectID = "p-1"
		recs[i].PrincipalID = "u-1"
		recs[i].RelayKeyHash = fmt.Sprintf("h-%d", i)
	}
	return recs
}

func distinctPieces(recs []payload.Record) int {
	seen := map[dedup.Hash]bool{}
	for _, r := range recs {
		if len(r.RequestBody) == 0 {
			continue
		}
		for _, p := range dedup.SplitBody(r.RequestBody).Pieces {
			seen[p.Hash] = true
		}
	}
	return len(seen)
}

func writeAll(t *testing.T, s *Sink, recs []payload.Record) {
	t.Helper()
	for _, r := range recs {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write %s: %v", r.RequestID, err)
		}
	}
}

func waitForRecord(t *testing.T, r payload.Reader, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := r.Get(context.Background(), id)
		if err == nil {
			return
		}
		if !errors.Is(err, payload.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("Get %s: %v", id, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertSameRecord(t *testing.T, got, want payload.Record) {
	t.Helper()
	if !bytes.Equal(got.RequestBody, want.RequestBody) || !bytes.Equal(got.ResponseBody, want.ResponseBody) {
		t.Fatalf("%s: bodies differ\n got req=%q resp=%q\nwant req=%q resp=%q",
			want.RequestID, got.RequestBody, got.ResponseBody, want.RequestBody, want.ResponseBody)
	}
	if got.RequestID != want.RequestID || !got.Timestamp.Equal(want.Timestamp) ||
		got.RequestTruncated != want.RequestTruncated || got.ResponseTruncated != want.ResponseTruncated ||
		got.ProjectID != want.ProjectID || got.PrincipalID != want.PrincipalID || got.RelayKeyHash != want.RelayKeyHash {
		t.Fatalf("%s: metadata differs\n got %+v\nwant %+v", want.RequestID, got, want)
	}
}

func countRows(t *testing.T, conn clickhouse.Conn, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestIntegration_DedupRoundTrip(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	ctx := context.Background()
	recs := conversationRecords(time.Now().UTC().Truncate(time.Microsecond))
	first, second := recs[:5], recs[5:9]

	// Two flushes from one sink: the second must not re-insert pieces the
	// first wrote today, so the raw row count is already the distinct count.
	s1, err := New(Config{DSN: dsn, WALDir: t.TempDir(), FlushInterval: 200 * time.Millisecond, Dedup: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeAll(t, s1, first)
	waitForRecord(t, s1, first[len(first)-1].RequestID)
	writeAll(t, s1, second)
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n, want := countRows(t, conn, "SELECT count() FROM payload_pieces"), distinctPieces(recs[:9]); n != uint64(want) {
		t.Fatalf("payload_pieces rows after one sink = %d, want %d", n, want)
	}

	// A fresh sink re-inserts shared pieces; the merge collapses them.
	s2, err := New(Config{DSN: dsn, WALDir: t.TempDir(), Dedup: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeAll(t, s2, recs[9:])
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	for _, want := range recs {
		got, err := rdr.Get(ctx, want.RequestID)
		if err != nil {
			t.Fatalf("Get %s: %v", want.RequestID, err)
		}
		assertSameRecord(t, got, want)
	}
	if _, err := rdr.Get(ctx, "req-missing"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	if err := conn.Exec(ctx, "OPTIMIZE TABLE payload_pieces FINAL"); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	if n, want := countRows(t, conn, "SELECT count() FROM payload_pieces"), distinctPieces(recs); n != uint64(want) {
		t.Fatalf("payload_pieces rows = %d, want %d distinct", n, want)
	}
	if n := countRows(t, conn, "SELECT count() FROM payload_logs"); n != 0 {
		t.Fatalf("dedup sink wrote %d payload_logs rows", n)
	}

	// A request row whose pieces are gone is an error, not a partial body.
	if err := conn.Exec(ctx, "TRUNCATE TABLE payload_pieces"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := rdr.Get(ctx, recs[0].RequestID); !errors.Is(err, dedup.ErrMissingPiece) {
		t.Fatalf("Get with missing pieces: err = %v, want ErrMissingPiece", err)
	}
}

// A hash list too long to send as query literals still rebuilds.
func TestIntegration_DedupManyPieces(t *testing.T) {
	dsn, _ := throwawayDatabase(t)
	messages := make([]string, 15_001)
	for i := range messages {
		messages[i] = fmt.Sprintf(`{"role":"user","content":"m%d"}`, i)
	}
	want := payload.Record{
		RequestID:   "req-many",
		Timestamp:   time.Now().UTC().Truncate(time.Microsecond),
		RequestBody: []byte(`{"model":"m","messages":[` + strings.Join(messages, ",") + `]}`),
	}
	if n := distinctPieces([]payload.Record{want}); n <= 15_000 {
		t.Fatalf("fixture has %d distinct pieces, want more than 15000", n)
	}

	s, err := New(Config{DSN: dsn, WALDir: t.TempDir(), Dedup: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeAll(t, s, []payload.Record{want})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	got, err := rdr.Get(context.Background(), want.RequestID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertSameRecord(t, got, want)
}

func TestIntegration_ReadsWholeBodyRowsAfterDedup(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	ctx := context.Background()
	recs := conversationRecords(time.Now().UTC().Truncate(time.Microsecond))
	whole, split := recs[0], recs[1]

	for _, c := range []struct {
		dedup bool
		rec   payload.Record
	}{{false, whole}, {true, split}} {
		s, err := New(Config{DSN: dsn, WALDir: t.TempDir(), Dedup: c.dedup})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		writeAll(t, s, []payload.Record{c.rec})
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	for _, want := range []payload.Record{whole, split} {
		got, err := rdr.Get(ctx, want.RequestID)
		if err != nil {
			t.Fatalf("Get %s: %v", want.RequestID, err)
		}
		assertSameRecord(t, got, want)
	}
	if n := countRows(t, conn, "SELECT count() FROM payload_logs WHERE request_id = ?", split.RequestID); n != 0 {
		t.Fatal("dedup sink wrote a payload_logs row")
	}
	if n := countRows(t, conn, "SELECT count() FROM payload_requests WHERE request_id = ?", whole.RequestID); n != 0 {
		t.Fatal("whole-body sink wrote a payload_requests row")
	}
}

func TestIntegration_DedupTablesTTL(t *testing.T) {
	_, conn := throwawayDatabase(t)
	ctx := context.Background()
	engine := func(table string) string {
		var e string
		if err := conn.QueryRow(ctx,
			"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table).Scan(&e); err != nil {
			t.Fatalf("engine of %s: %v", table, err)
		}
		return e
	}

	if err := ensureSchema(ctx, conn, 7); err != nil {
		t.Fatalf("ensureSchema(7): %v", err)
	}
	for table, ttl := range map[string]string{
		"payload_logs":     "TTL toDateTime(ts) + toIntervalDay(7)",
		"payload_requests": "TTL toDateTime(ts) + toIntervalDay(7)",
		"payload_pieces":   "TTL toDateTime(last_seen) + toIntervalDay(8)",
	} {
		if e := engine(table); !strings.Contains(e, ttl) {
			t.Fatalf("%s engine = %q, want %q", table, e, ttl)
		}
	}

	if err := ensureSchema(ctx, conn, 0); err != nil {
		t.Fatalf("ensureSchema(0): %v", err)
	}
	for _, table := range []string{"payload_logs", "payload_requests", "payload_pieces"} {
		if e := engine(table); strings.Contains(e, " TTL ") {
			t.Fatalf("%s keeps a TTL after retention 0: %q", table, e)
		}
	}
}
