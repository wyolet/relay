//go:build integration

// Live ClickHouse round-trip smoke. Runs only with -tags=integration AND
// RELAY_CH_DSN set (else skipped). Validates the parts the wal unit tests
// can't: schema DDL, insert column mapping, body round-trip, the bloom-index
// Get, the body-stripping List projection, and keyset pagination against a
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

	recs := []payload.Record{
		{
			RequestID: marker + "-1", Source: "pipeline", Timestamp: now,
			Status: 200, Streamed: true, ModelID: marker, PolicyID: "p1",
			RequestBody:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			ResponseBody: []byte(`{"choices":[{"message":{"content":"hello"}}]}`),
		},
		{
			RequestID: marker + "-2", Source: "pipeline", Timestamp: now.Add(time.Second),
			Status: 500, ModelID: marker, PolicyID: "p1", ErrorKind: "upstream_error",
			RequestBody:      []byte(`{"messages":[]}`),
			RequestTruncated: true,
		},
	}
	for _, r := range recs {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	ctx := context.Background()
	q := payload.Query{Since: time.Hour, ModelID: []string{marker}, Limit: 10}

	// Wait for the flush tick (segment → CH).
	var list []payload.Record
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		list, err = s.List(ctx, q)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) == 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 records, got %d", len(list))
	}

	// Newest-first; List must strip bodies but keep metadata + truncation flag.
	if list[0].RequestID != marker+"-2" || list[1].RequestID != marker+"-1" {
		t.Fatalf("not newest-first: %s, %s", list[0].RequestID, list[1].RequestID)
	}
	if list[0].RequestBody != nil || list[0].ResponseBody != nil {
		t.Fatalf("List leaked bodies: %+v", list[0])
	}
	if !list[0].RequestTruncated || list[0].ErrorKind != "upstream_error" || list[0].Status != 500 {
		t.Fatalf("metadata round-trip mismatch: %+v", list[0])
	}

	// Get returns full bodies.
	full, err := s.Get(ctx, marker+"-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(full.RequestBody) != `{"messages":[{"role":"user","content":"hi"}]}` ||
		string(full.ResponseBody) != `{"choices":[{"message":{"content":"hello"}}]}` {
		t.Fatalf("body round-trip mismatch: req=%q resp=%q", full.RequestBody, full.ResponseBody)
	}

	// Get of an unknown id → ErrNotFound.
	if _, err := s.Get(ctx, marker+"-missing"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Status filter pushed into SQL.
	errOnly, err := s.List(ctx, payload.Query{Since: time.Hour, ModelID: []string{marker}, StatusMin: 400})
	if err != nil {
		t.Fatalf("List status filter: %v", err)
	}
	if len(errOnly) != 1 || errOnly[0].RequestID != marker+"-2" {
		t.Fatalf("status filter: %+v", errOnly)
	}

	// Keyset pagination: page size 1 walks newest→oldest with no repeats.
	pq := payload.Query{Since: time.Hour, ModelID: []string{marker}, Limit: 1}
	p1, err := s.List(ctx, pq)
	if err != nil || len(p1) != 1 || p1[0].RequestID != marker+"-2" {
		t.Fatalf("page1: err=%v rows=%+v", err, p1)
	}
	pq.CursorTS, pq.CursorID = p1[0].Timestamp, p1[0].RequestID
	p2, err := s.List(ctx, pq)
	if err != nil || len(p2) != 1 || p2[0].RequestID != marker+"-1" {
		t.Fatalf("page2: err=%v rows=%+v", err, p2)
	}
	pq.CursorTS, pq.CursorID = p2[0].Timestamp, p2[0].RequestID
	if p3, err := s.List(ctx, pq); err != nil || len(p3) != 0 {
		t.Fatalf("page3 should be empty: err=%v rows=%+v", err, p3)
	}

	// The standalone read-only Reader sees the same rows.
	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	rl, err := rdr.List(ctx, q)
	if err != nil || len(rl) != 2 {
		t.Fatalf("reader List: err=%v rows=%d", err, len(rl))
	}
}
