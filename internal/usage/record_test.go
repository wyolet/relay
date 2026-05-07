package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wyolet/relay/pkg/eventlog"
)

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return sr, tp
}

func newTestLC(tp *sdktrace.TracerProvider) *Lifecycle {
	tracer := tp.Tracer("test")
	_, sp := tracer.Start(context.Background(), SpanName)
	lc := &Lifecycle{
		RequestID:    "req-1",
		Model:        "gpt-4o",
		Provider:     "openai",
		Pool:         "pool-a",
		SecretHash:   SecretHash("sk-secret"),
		TerminatedBy: TerminatedClean,
		Tokens:       TokenBlock{Prompt: 10, Completion: 20, Total: 30, Cached: 5},
		Attempts: []Attempt{
			{SecretHash: SecretHash("sk-secret"), Outcome: "success", LatencyMS: 100},
		},
		Attribution: map[string]string{"user_id": "u1"},
		Metrics:     map[string]int64{"retry_count": 1},
		StartedAt:   time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC),
		EndedAt:     time.Date(2026, 5, 5, 10, 0, 1, 0, time.UTC),
		InstanceID:  "pod-1",
		RelayVersion: "v0.1.0",
	}
	lc.SetSpan(sp)
	return lc
}

func TestRecord_HappyPath(t *testing.T) {
	dir := t.TempDir()
	el, err := eventlog.New(eventlog.Config{Dir: dir, BufferSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close(context.Background())

	sr, tp := newRecorder()
	otel.SetTracerProvider(tp)
	orig := defaultEventLogger
	defaultEventLogger = el
	defer func() { defaultEventLogger = orig }()

	lc := newTestLC(tp)
	Record(context.Background(), lc)

	// Flush eventlog.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	el.Close(ctx)

	// Read JSONL file.
	entries, err := readJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 event, got %d", len(entries))
	}
	ev := entries[0]
	assertStr(t, ev, "request_id", "req-1")
	assertStr(t, ev, "model", "gpt-4o")
	assertStr(t, ev, "provider", "openai")
	assertStr(t, ev, "pool", "pool-a")
	assertStr(t, ev, "terminated_by", "clean")
	assertStr(t, ev, "instance_id", "pod-1")
	assertStr(t, ev, "relay_version", "v0.1.0")
	if v, ok := ev["event_version"].(float64); !ok || int(v) != 1 {
		t.Errorf("event_version want 1, got %v", ev["event_version"])
	}
	tokens, _ := ev["tokens"].(map[string]interface{})
	if tokens == nil {
		t.Fatal("missing tokens")
	}

	// Check span.
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Status().Code != codes.Ok {
		t.Errorf("span status want Ok, got %v", sp.Status().Code)
	}
	attrMap := spanAttrs(sp)
	if attrMap["relay.request_id"] != "req-1" {
		t.Errorf("relay.request_id = %v", attrMap["relay.request_id"])
	}
	if attrMap["relay.tokens.cached"] != int64(5) {
		t.Errorf("relay.tokens.cached = %v", attrMap["relay.tokens.cached"])
	}
	if attrMap["relay.metrics.retry_count"] != int64(1) {
		t.Errorf("relay.metrics.retry_count = %v", attrMap["relay.metrics.retry_count"])
	}
	if attrMap["relay.attr.user_id"] != "u1" {
		t.Errorf("relay.attr.user_id = %v", attrMap["relay.attr.user_id"])
	}
	if _, ok := attrMap["relay.attempts"]; !ok {
		t.Error("missing relay.attempts attribute")
	}
}

func TestRecord_NonClean(t *testing.T) {
	cases := []TerminatedBy{
		TerminatedClientCancel,
		TerminatedUpstreamError,
		TerminatedUpstreamTimeout,
		TerminatedRateLimited,
		TerminatedPoolExhausted,
		TerminatedRelayError,
	}
	for _, tb := range cases {
		t.Run(string(tb), func(t *testing.T) {
			sr, tp := newRecorder()
			lc := newTestLC(tp)
			lc.TerminatedBy = tb

			orig := defaultEventLogger
			defaultEventLogger = nil
			defer func() { defaultEventLogger = orig }()

			Record(context.Background(), lc)

			spans := sr.Ended()
			if len(spans) != 1 {
				t.Fatalf("want 1 span, got %d", len(spans))
			}
			sp := spans[0]
			if sp.Status().Code != codes.Error {
				t.Errorf("want Error status, got %v", sp.Status().Code)
			}
			if sp.Status().Description != string(tb) {
				t.Errorf("want description %q, got %q", tb, sp.Status().Description)
			}
		})
	}
}

func TestRecord_OmitEmpty(t *testing.T) {
	dir := t.TempDir()
	el, _ := eventlog.New(eventlog.Config{Dir: dir, BufferSize: 64})
	defer el.Close(context.Background())

	_, tp := newRecorder()
	lc := &Lifecycle{
		RequestID:    "req-2",
		TerminatedBy: TerminatedClean,
		StartedAt:    time.Now(),
		EndedAt:      time.Now(),
	}
	tracer := tp.Tracer("test")
	_, sp := tracer.Start(context.Background(), SpanName)
	lc.SetSpan(sp)

	orig := defaultEventLogger
	defaultEventLogger = el
	defer func() { defaultEventLogger = orig }()

	Record(context.Background(), lc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	el.Close(ctx)

	entries, err := readJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 event, got %d", len(entries))
	}
	ev := entries[0]
	if _, ok := ev["attribution"]; ok {
		t.Error("attribution should be omitted when nil")
	}
	if _, ok := ev["metrics"]; ok {
		t.Error("metrics should be omitted when nil")
	}
	if _, ok := ev["attempts"]; ok {
		t.Error("attempts should be omitted when nil")
	}
}

func TestRecord_EventlogFull(t *testing.T) {
	dir := t.TempDir()
	el, _ := eventlog.New(eventlog.Config{Dir: dir, BufferSize: 1})
	el.Close(context.Background())

	_, tp := newRecorder()

	orig := defaultEventLogger
	defaultEventLogger = el
	defer func() { defaultEventLogger = orig }()

	before := testutil.ToFloat64(metricDroppedEvents)
	for i := 0; i < 5; i++ {
		lc := newTestLC(tp)
		Record(context.Background(), lc)
	}
	after := testutil.ToFloat64(metricDroppedEvents)
	if after-before < 5 {
		t.Errorf("metricDroppedEvents should have increased by at least 5, before=%v after=%v", before, after)
	}
}

func TestRecord_NilLogger(t *testing.T) {
	sr, tp := newRecorder()
	lc := newTestLC(tp)

	orig := defaultEventLogger
	defaultEventLogger = nil
	defer func() { defaultEventLogger = orig }()

	// Must not panic.
	Record(context.Background(), lc)

	if len(sr.Ended()) != 1 {
		t.Error("span should still be ended when logger is nil")
	}
}

func TestSecretHash(t *testing.T) {
	h := SecretHash("my-secret-key")
	if len(h) != 12 {
		t.Errorf("SecretHash length want 12, got %d", len(h))
	}
	// Deterministic.
	if SecretHash("my-secret-key") != h {
		t.Error("SecretHash is not deterministic")
	}
	// Different inputs differ.
	if SecretHash("other-key") == h {
		t.Error("different inputs should produce different hashes")
	}
	// Hex charset.
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char in hash: %c", c)
		}
	}
}

func TestResolveInstanceID(t *testing.T) {
	t.Run("hint override", func(t *testing.T) {
		got := resolveInstanceIDFallback("my-pod-42")
		if got != "my-pod-42" {
			t.Errorf("want my-pod-42, got %s", got)
		}
	})
	t.Run("fallback hostname", func(t *testing.T) {
		got := resolveInstanceIDFallback("")
		h, _ := os.Hostname()
		if h != "" && got != h {
			t.Errorf("want hostname %s, got %s", h, got)
		}
	})
}

// helpers

func readJSONL(dir string) ([]map[string]interface{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(dir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var m map[string]interface{}
			if err := json.Unmarshal(line, &m); err != nil {
				f.Close()
				return nil, err
			}
			out = append(out, m)
		}
		f.Close()
	}
	return out, nil
}

func assertStr(t *testing.T, m map[string]interface{}, key, want string) {
	t.Helper()
	got, ok := m[key].(string)
	if !ok {
		t.Errorf("key %q missing or not string, got %T=%v", key, m[key], m[key])
		return
	}
	if got != want {
		t.Errorf("key %q: want %q, got %q", key, want, got)
	}
}

func spanAttrs(sp sdktrace.ReadOnlySpan) map[string]interface{} {
	out := make(map[string]interface{}, len(sp.Attributes()))
	for _, a := range sp.Attributes() {
		switch a.Value.Type().String() {
		case "STRING":
			out[string(a.Key)] = a.Value.AsString()
		case "INT64":
			out[string(a.Key)] = a.Value.AsInt64()
		case "FLOAT64":
			out[string(a.Key)] = a.Value.AsFloat64()
		case "BOOL":
			out[string(a.Key)] = a.Value.AsBool()
		default:
			out[string(a.Key)] = a.Value.AsInterface()
		}
	}
	return out
}
