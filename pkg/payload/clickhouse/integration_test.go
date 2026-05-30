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
