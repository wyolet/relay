//go:build integration

// Live ClickHouse round-trip smoke. Runs only with -tags=integration AND
// RELAY_CH_DSN set (else skipped). Validates the parts the wal unit tests
// can't: schema DDL, insert column mapping, and reader SQL against a real
// server.
//
//	RELAY_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/usage/clickhouse/ -run Integration -v
package clickhouse

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
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

	// Unique marker so repeated runs (and any other rows) don't interfere.
	marker := "smoke-" + time.Now().Format("20060102T150405.000000000")
	now := time.Now().UTC().Truncate(time.Microsecond)
	// RELAY_CH_DSN often points at a shared dev ClickHouse; without this the
	// marker rows leak into live usage stats as a phantom "smoke-*" model.
	t.Cleanup(func() { deleteMarkerRows(t, dsn, "usage_events", "model_id", marker) })

	events := []usage.Event{
		{
			RequestID: marker + "-1", Source: "pipeline", Timestamp: now,
			Status: 200, DurationMs: 123, Streamed: true, FinishReason: "stop",
			Attempts: 1, ModelID: marker, PolicyID: "p1",
			Upstream: &sdkusage.UpstreamTiming{Start: 100, ResponseStart: 200, ResponseEnd: 300},
			Tokens:   sdkusage.Tokens{"input": 10, "output": 5},
			Extras:   map[string]string{"client_ip": "1.2.3.4"},
		},
		{
			RequestID: marker + "-2", Source: "pipeline", Timestamp: now.Add(time.Second),
			Status: 500, DurationMs: 50, ModelID: marker, PolicyID: "p1",
			ErrorKind: "upstream_5xx",
		},
	}
	for _, ev := range events {
		if err := s.Write(ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Wait for a flush tick (segment → CH). async_insert with
	// wait_for_async_insert=1 makes rows queryable once Send returns.
	q := usage.EventQuery{Since: time.Hour, ModelID: []string{marker}, Limit: 10}
	var got []usage.Event
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err = s.Events(context.Background(), q)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) == 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events back, got %d", len(got))
	}

	// Newest-first; verify column mapping round-tripped.
	if got[0].RequestID != marker+"-2" || got[1].RequestID != marker+"-1" {
		t.Fatalf("not newest-first: %s, %s", got[0].RequestID, got[1].RequestID)
	}
	e1 := got[1]
	if e1.Status != 200 || e1.DurationMs != 123 || !e1.Streamed || e1.FinishReason != "stop" {
		t.Fatalf("scalar round-trip mismatch: %+v", e1)
	}
	if e1.Upstream == nil || e1.Upstream.ResponseEnd != 300 {
		t.Fatalf("upstream round-trip mismatch: %+v", e1.Upstream)
	}
	if e1.Tokens["input"] != 10 || e1.Tokens["output"] != 5 {
		t.Fatalf("tokens round-trip mismatch: %+v", e1.Tokens)
	}
	if e1.Extras["client_ip"] != "1.2.3.4" {
		t.Fatalf("extras round-trip mismatch: %+v", e1.Extras)
	}
	if got[0].Upstream != nil {
		t.Fatalf("event-2 had no upstream timing; sentinel should map to nil, got %+v", got[0].Upstream)
	}

	// Summary aggregation pushed into SQL.
	res, err := s.Summary(context.Background(), usage.SummaryQuery{
		EventQuery: q, GroupBy: "model_id",
	})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 group, got %d", len(res.Rows))
	}
	row := res.Rows[0]
	if row.Group["model_id"] != marker {
		t.Fatalf("group key: %+v", row.Group)
	}
	if row.Requests != 2 || row.ErrorCount != 1 {
		t.Fatalf("requests/errors: %d/%d", row.Requests, row.ErrorCount)
	}
	if row.Tokens["input"] != 10 || row.Tokens["output"] != 5 {
		t.Fatalf("summary tokens: %+v", row.Tokens)
	}

	// TimeSeries pushed into SQL: both events in one hour-bucket.
	ts, err := s.TimeSeries(context.Background(), usage.TimeSeriesQuery{EventQuery: q, Interval: time.Hour})
	if err != nil {
		t.Fatalf("TimeSeries: %v", err)
	}
	if len(ts.Rows) != 1 || ts.Rows[0].Group != nil {
		t.Fatalf("want 1 ungrouped series, got %d rows: %+v", len(ts.Rows), ts.Rows)
	}
	var reqs, errs int64
	tokens := map[string]int64{}
	for _, p := range ts.Rows[0].Points {
		reqs += p.Requests
		errs += p.ErrorCount
		for k, v := range p.Tokens {
			tokens[k] += v
		}
	}
	if reqs != 2 || errs != 1 {
		t.Fatalf("timeseries totals: reqs=%d errs=%d", reqs, errs)
	}
	if tokens["input"] != 10 || tokens["output"] != 5 {
		t.Fatalf("timeseries tokens: %+v", tokens)
	}

	tsg, err := s.TimeSeries(context.Background(), usage.TimeSeriesQuery{EventQuery: q, Interval: time.Hour, GroupBy: "model_id"})
	if err != nil {
		t.Fatalf("TimeSeries grouped: %v", err)
	}
	if len(tsg.Rows) != 1 || tsg.Rows[0].Group["model_id"] != marker {
		t.Fatalf("grouped timeseries: %+v", tsg.Rows)
	}

	// Keyset pagination: page size 1 walks newest→oldest with no repeats.
	pq := usage.EventQuery{Since: time.Hour, ModelID: []string{marker}, Limit: 1}
	p1, err := s.Events(context.Background(), pq)
	if err != nil || len(p1) != 1 || p1[0].RequestID != marker+"-2" {
		t.Fatalf("page1: err=%v rows=%+v", err, p1)
	}
	pq.CursorTS, pq.CursorID = p1[0].Timestamp, p1[0].RequestID
	p2, err := s.Events(context.Background(), pq)
	if err != nil || len(p2) != 1 || p2[0].RequestID != marker+"-1" {
		t.Fatalf("page2: err=%v rows=%+v", err, p2)
	}
	pq.CursorTS, pq.CursorID = p2[0].Timestamp, p2[0].RequestID
	if p3, err := s.Events(context.Background(), pq); err != nil || len(p3) != 0 {
		t.Fatalf("page3 should be empty: err=%v rows=%+v", err, p3)
	}
}

// deleteMarkerRows removes this run's synthetic rows so a shared ClickHouse
// (the common RELAY_CH_DSN target) isn't left with phantom usage data. A
// fresh connection is dialled because the Sink's own conn is closed by the
// time t.Cleanup fires. Best-effort: a delete failure logs, never fails the
// test.
func deleteMarkerRows(t *testing.T, dsn, table, column, marker string) {
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
	q := fmt.Sprintf("DELETE FROM %s WHERE %s LIKE ?", table, column)
	if err := conn.Exec(context.Background(), q, marker+"%"); err != nil {
		t.Logf("cleanup: delete %s rows: %v", table, err)
	}
}
