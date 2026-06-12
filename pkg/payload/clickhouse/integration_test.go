//go:build integration

// Live ClickHouse round-trip smoke. Runs only with -tags=integration AND
// RELAY_CH_DSN set (else skipped). Validates the schema DDL, insert column
// mapping, body round-trip, the bloom-index Get, and ErrNotFound against a
// real server.
//
//	RELAY_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/payload/clickhouse/ -run Integration -v
package clickhouse

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/payload"
)

func TestIntegration_RoundTrip(t *testing.T) {
	dsn := os.Getenv("RELAY_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_CH_DSN unset; skipping live ClickHouse smoke")
	}

	s, err := New(Config{
		DSN:           dsn,
		WALDir:        t.TempDir(),
		FlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	marker := "smoke-" + time.Now().Format("20060102T150405.000000000")
	// RELAY_CH_DSN often points at a shared dev ClickHouse; without this the
	// marker rows leak into the live payload log.
	t.Cleanup(func() { deleteMarkerRows(t, dsn, marker) })
	now := time.Now().UTC().Truncate(time.Microsecond)

	rec := payload.Record{
		RequestID:        marker + "-1",
		Timestamp:        now,
		RequestBody:      []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		ResponseBody:     []byte(`{"choices":[{"message":{"content":"hello"}}]}`),
		RequestTruncated: true,
	}
	if err := s.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	ctx := context.Background()

	// Wait for the flush tick (segment → CH), then Get the bodies back.
	var got payload.Record
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err = s.Get(ctx, marker+"-1")
		if err == nil {
			break
		}
		if !errors.Is(err, payload.ErrNotFound) {
			t.Fatalf("Get: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Get never landed: %v", err)
	}
	if string(got.RequestBody) != string(rec.RequestBody) ||
		string(got.ResponseBody) != string(rec.ResponseBody) {
		t.Fatalf("body round-trip mismatch: req=%q resp=%q", got.RequestBody, got.ResponseBody)
	}
	if !got.RequestTruncated || got.ResponseTruncated {
		t.Fatalf("truncation flags mismatch: %+v", got)
	}

	if _, err := s.Get(ctx, marker+"-missing"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// The standalone read-only Reader sees the same row.
	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	if _, err := rdr.Get(ctx, marker+"-1"); err != nil {
		t.Fatalf("reader Get: %v", err)
	}
}

// deleteMarkerRows removes this run's synthetic rows so a shared ClickHouse
// (the common RELAY_CH_DSN target) isn't left with phantom payload data. A
// fresh connection is dialled because the Sink's own conn is closed by the
// time t.Cleanup fires. Best-effort: a delete failure logs, never fails the
// test.
func deleteMarkerRows(t *testing.T, dsn, marker string) {
	t.Helper()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Logf("cleanup: parse dsn: %v", err)
		return
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Logf("cleanup: open: %v", err)
		return
	}
	defer conn.Close()
	if err := conn.Exec(context.Background(), "DELETE FROM payload_logs WHERE request_id LIKE ?", marker+"%"); err != nil {
		t.Logf("cleanup: delete payload_logs rows: %v", err)
	}
}
