package clickhouse

import (
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// sampleEvents covers every column shape: set and unset maps, priced, priced
// at zero and unpriced cost, with and without upstream timing, a zero
// timestamp, and a non-UTC one.
func sampleEvents() []usage.Event {
	zero := int64(0)
	priced := int64(7_500_000)
	tashkent := time.FixedZone("UTC+5", 5*3600)
	return []usage.Event{
		{
			RequestID: "req-full", Source: "pipeline",
			Timestamp: time.Date(2026, 10, 26, 8, 30, 1, 123456789, time.UTC),
			Status:    200, DurationMs: 123, Streamed: true, FinishReason: "stop",
			Attempts: 2, ErrorKind: "", ErrorMessage: "",
			Upstream:     &sdkusage.UpstreamTiming{Start: 100, ResponseStart: 200, ResponseEnd: 300},
			RelayKeyHash: "hash-1", PolicyID: "pol-1", ModelID: "mod-1", RequestedModel: "gpt-x",
			HostID: "host-1", HostKeyID: "hk-1",
			Tokens: sdkusage.Tokens{"input": 10, "output": 5, "cache_read": 1 << 40},
			Extras: map[string]string{"client_ip": "1.2.3.4", "ua": "curl/8"},
			Tags:   map[string]string{"session_id": "s1", "leg": "bytepass"},
			Model:  "gpt-x", Host: "host-a", Policy: "default", Provider: "vendor-a",
			CostNanos:     &priced,
			CostBreakdown: map[string]int64{"tokens.input": 5_000_000, "tokens.output": 2_500_000},
			Pricing:       "host-a-gpt-x",
			ProjectID:     "proj-1", Project: "indexer", TeamID: "team-1", Team: "search",
			PrincipalKind: "service_account", PrincipalID: "sa-1", Principal: "indexer-bot",
			CredentialKind: "key", CredentialID: "key-1", RelayKey: "indexer-prod",
		},
		{},
		{
			RequestID: "req-free", Source: "proxy", Timestamp: time.Date(2026, 10, 26, 13, 0, 0, 0, tashkent),
			Status: 200, CostNanos: &zero,
			Tokens: sdkusage.Tokens{}, Extras: map[string]string{}, Tags: map[string]string{},
			Model: "gpt-x", Host: "host-b", Provider: "vendor-a",
		},
		{
			RequestID: "req-rejected", Source: "pipeline", Timestamp: time.Unix(1_800_000_000, 1),
			ErrorKind: "rate_limited", ErrorMessage: "too many requests — retry later",
			Tags: map[string]string{"leg": "translate"}, Model: "gpt-x", RelayKey: "indexer-prod",
		},
		{
			RequestID: "req-5xx", Source: "batch", Timestamp: time.Unix(1_800_000_001, 0).UTC(),
			Status: 503, DurationMs: 50, Attempts: 3, ErrorKind: "upstream_5xx",
			Upstream:      &sdkusage.UpstreamTiming{Start: 0, ResponseStart: 0, ResponseEnd: 0},
			Tokens:        sdkusage.Tokens{"input": 7},
			CostBreakdown: map[string]int64{"tokens.input": 0},
			Model:         "claude-y", Host: "host-c", Provider: "vendor-b",
		},
	}
}

// columnRow reads row r of col as the Go value insertValues gives for it,
// with timestamps as their stored DateTime64 ticks.
func columnRow(t *testing.T, col proto.ColInput, r int) any {
	t.Helper()
	switch c := col.(type) {
	case *proto.ColStr:
		return c.Row(r)
	case *proto.ColLowCardinality[string]:
		return c.Row(r)
	case *proto.ColDateTime64:
		return c.Data[r]
	case *proto.ColUInt16:
		return c.Row(r)
	case *proto.ColUInt8:
		return c.Row(r)
	case *proto.ColInt64:
		return c.Row(r)
	case *proto.ColMap[string, int64]:
		return c.Row(r)
	case *proto.ColMap[string, string]:
		return c.Row(r)
	}
	t.Fatalf("unhandled column type %T", col)
	return nil
}

func wantRow(ev usage.Event) []any {
	want := insertValues(ev)
	for i, v := range want {
		if ts, ok := v.(time.Time); ok {
			want[i] = proto.ToDateTime64(ts, proto.PrecisionNano)
		}
	}
	return want
}

func checkRows(t *testing.T, cols *eventColumns, events []usage.Event) {
	t.Helper()
	for r, ev := range events {
		want := wantRow(ev)
		for i, col := range cols.input {
			if got := columnRow(t, col.Data, r); !reflect.DeepEqual(got, want[i]) {
				t.Errorf("row %d (%s) column %s: got %#v, want %#v", r, ev.RequestID, col.Name, got, want[i])
			}
		}
	}
}

func TestEventColumnsInputMatchesInsertColumns(t *testing.T) {
	cols := newEventColumns()
	if len(cols.input) != len(insertColumns) {
		t.Fatalf("%d input columns for %d insertColumns", len(cols.input), len(insertColumns))
	}
	for i, col := range cols.input {
		if col.Name != insertColumns[i] {
			t.Errorf("input column %d is %q, want %q", i, col.Name, insertColumns[i])
		}
	}
}

func TestEventColumnsTypesMatchCreateTable(t *testing.T) {
	for _, col := range newEventColumns().input {
		typ := string(col.Data.Type())
		decl := regexp.MustCompile(`\n\s+` + col.Name + `\s+` + regexp.QuoteMeta(typ) + `[\s,]`)
		if !decl.MatchString(createTableSQL) {
			t.Errorf("column %s has type %s, which createTableSQL does not declare", col.Name, typ)
		}
	}
}

func TestEventColumnsMatchInsertValues(t *testing.T) {
	events := sampleEvents()
	cols := newEventColumns()
	cols.appendEvents(events)
	checkRows(t, cols, events)

	// A reused set holds only the next batch.
	cols.reset()
	reversed := make([]usage.Event, len(events))
	for i, ev := range events {
		reversed[len(events)-1-i] = ev
	}
	cols.appendEvents(reversed)
	if got := cols.input[0].Data.Rows(); got != len(reversed) {
		t.Fatalf("after reset: %d rows, want %d", got, len(reversed))
	}
	checkRows(t, cols, reversed)
}

// The wire block survives a decode, including the LowCardinality dictionaries
// a reused set rebuilds per batch.
func TestEventColumnsEncodeRoundTrip(t *testing.T) {
	events := sampleEvents()
	cols := newEventColumns()
	var buf proto.Buffer
	for batch := range 2 {
		cols.reset()
		cols.appendEvents(events[batch:])
		buf.Reset()
		block := proto.Block{Columns: len(cols.input), Rows: len(events) - batch}
		if err := block.EncodeRawBlock(&buf, proto.Version, cols.input); err != nil {
			t.Fatalf("encode: %v", err)
		}

		decoded := newEventColumns()
		results := make(proto.Results, len(decoded.input))
		for i, col := range decoded.input {
			results[i] = proto.ResultColumn{Name: col.Name, Data: col.Data.(proto.ColResult)}
		}
		var got proto.Block
		if err := got.DecodeRawBlock(proto.NewReader(&buf), proto.Version, results); err != nil {
			t.Fatalf("decode: %v", err)
		}
		checkRows(t, decoded, events[batch:])
	}
}

func BenchmarkEventColumns(b *testing.B) {
	sample := sampleEvents()
	events := make([]usage.Event, 1000)
	for i := range events {
		events[i] = sample[i%len(sample)]
	}
	cols := newEventColumns()
	var buf proto.Buffer
	b.ReportAllocs()
	for b.Loop() {
		cols.appendEvents(events)
		buf.Reset()
		block := proto.Block{Columns: len(cols.input), Rows: len(events)}
		if err := block.EncodeRawBlock(&buf, proto.Version, cols.input); err != nil {
			b.Fatal(err)
		}
		cols.reset()
	}
}
