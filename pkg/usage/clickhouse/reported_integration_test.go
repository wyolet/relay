//go:build integration

// Live ClickHouse round trip of usage events recorded from client telemetry (source "otlp"). Runs only with -tags=integration AND RELAY_TEST_CH_DSN set (else skipped).
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/usage/clickhouse/ -run Integration -v

package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func TestIntegration_ReportedEventRoundTrip(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse smoke")
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

	// A reported event may name no catalog model, so the rows of this run are told apart by project.
	marker := "smoke-" + time.Now().Format("20060102T150405.000000000")
	now := time.Now().UTC().Truncate(time.Microsecond)
	t.Cleanup(func() { deleteMarkerRows(t, dsn, "usage_events", "project_id", marker) })

	traceID := randomHex(t, 16)
	// The two identities the receiver records under: the ids of a span, and a digest of a response id.
	spanCall := "otlp-" + traceID + "-" + randomHex(t, 8)
	responseCall := "otlp-" + randomHex(t, 16) + "-response"

	unpriced := usage.Event{
		RequestID: spanCall, Source: "otlp", Timestamp: now,
		Status: 200, DurationMs: 1500, Streamed: true, FinishReason: "stop",
		RequestedModel: "a-model-the-catalog-does-not-know",
		ProjectID:      marker, Project: "agents", PrincipalKind: "serviceaccount", PrincipalID: "sa-1", Principal: "billing-agent",
		CredentialKind: "key", CredentialID: "key-1", RelayKeyHash: "h-1",
		Upstream: &sdkusage.UpstreamTiming{ResponseStart: 250_000, ResponseEnd: 1_500_000},
		Tokens: sdkusage.Tokens{
			"input": 176, "output": 300, "cache_read": 1024, "cache_creation": 12,
			"reasoning": 128, "audio_input": 40, "audio_output": 16,
		},
		Extras: map[string]string{
			"telemetry_convention": "gen_ai",
			"trace_id":             traceID,
			"span_id":              spanCall[len(spanCall)-16:],
			"response_id":          "resp_123",
			"session_id":           "conv_9",
			"service":              "billing-agent",
			"operation":            "chat",
			"reported_provider":    "example-provider",
		},
	}
	priced := usage.Event{
		RequestID: responseCall, Source: "otlp", Timestamp: now.Add(time.Second),
		Status: 200, RequestedModel: "example-model", Model: "example-model", Provider: "example", Pricing: "example-model-list",
		ProjectID: marker, Project: "agents",
		Tokens:    sdkusage.Tokens{"input": 10, "output": 5},
		Extras:    map[string]string{"telemetry_convention": "gen_ai", "response_id": "resp_detached_1"},
		CostNanos: costPtr(105_000), CostBreakdown: map[string]int64{"tokens.input": 30_000, "tokens.output": 75_000},
	}
	proxied := usage.Event{
		RequestID: marker + "-proxied", Source: "pipeline", Timestamp: now.Add(2 * time.Second),
		Status: 200, ProjectID: marker, Project: "agents",
		Tokens:    sdkusage.Tokens{"input": 7, "output": 3},
		CostNanos: costPtr(0),
	}
	for _, ev := range []usage.Event{unpriced, priced, proxied} {
		if err := s.Write(ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	ctx := context.Background()
	all := usage.EventQuery{Since: time.Hour, ProjectID: []string{marker}, Limit: 10}
	reported := all
	reported.Source = []string{"otlp"}

	var got []usage.Event
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if got, err = s.Events(ctx, all); err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) == 3 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events back, got %d", len(got))
	}

	if got, err = s.Events(ctx, reported); err != nil {
		t.Fatalf("Events source=otlp: %v", err)
	}
	if len(got) != 2 || got[0].RequestID != responseCall || got[1].RequestID != spanCall {
		t.Fatalf("source=otlp: want the two reported events newest first, got %+v", got)
	}
	elsewhere := all
	elsewhere.Source = []string{"pipeline"}
	if rest, err := s.Events(ctx, elsewhere); err != nil || len(rest) != 1 || rest[0].RequestID != proxied.RequestID {
		t.Fatalf("source=pipeline: err=%v rows=%+v", err, rest)
	}

	e := got[1]
	if e.Source != "otlp" || e.Status != 200 || e.DurationMs != 1500 || !e.Streamed || e.FinishReason != "stop" || !e.Timestamp.Equal(now) {
		t.Errorf("scalar round-trip mismatch: %+v", e)
	}
	if e.CostNanos != nil || len(e.CostBreakdown) != 0 || e.Pricing != "" {
		t.Errorf("unpriced event came back priced: cost=%v breakdown=%v pricing=%q", e.CostNanos, e.CostBreakdown, e.Pricing)
	}
	if !maps.Equal(e.Tokens, unpriced.Tokens) {
		t.Errorf("tokens = %v, want %v", e.Tokens, unpriced.Tokens)
	}
	if !maps.Equal(e.Extras, unpriced.Extras) {
		t.Errorf("extras = %v, want %v", e.Extras, unpriced.Extras)
	}
	if e.ModelID != "" || e.Model != "" || e.HostID != "" || e.PolicyID != "" || e.RequestedModel != unpriced.RequestedModel {
		t.Errorf("model and routing fields = %+v, want only the requested model", e)
	}
	if e.ProjectID != marker || e.Project != "agents" || e.PrincipalKind != "serviceaccount" || e.PrincipalID != "sa-1" || e.CredentialKind != "key" || e.RelayKeyHash != "h-1" {
		t.Errorf("attribution round-trip mismatch: %+v", e)
	}
	if e.Upstream == nil || e.Upstream.ResponseStart != 250_000 || e.Upstream.ResponseEnd != 1_500_000 {
		t.Errorf("upstream timing = %+v", e.Upstream)
	}
	if p := got[0]; p.CostNanos == nil || *p.CostNanos != 105_000 || p.Pricing != "example-model-list" || p.Extras["response_id"] != "resp_detached_1" {
		t.Errorf("priced reported event = %+v", p)
	}

	single := usage.EventQuery{Since: time.Hour, RequestID: spanCall, Limit: 1}
	if one, err := s.Events(ctx, single); err != nil || len(one) != 1 || one[0].RequestID != spanCall {
		t.Fatalf("lookup by request id: err=%v rows=%+v", err, one)
	}

	bySource, err := s.Summary(ctx, usage.SummaryQuery{EventQuery: all, GroupBy: "source"})
	if err != nil {
		t.Fatalf("Summary by source: %v", err)
	}
	if len(bySource.Rows) != 2 {
		t.Fatalf("source groups: want 2 (otlp + pipeline), got %+v", bySource.Rows)
	}
	for _, row := range bySource.Rows {
		switch row.Group["source"] {
		case "otlp":
			// The unpriced event is counted as unpriced and adds nothing to the cost.
			if row.Requests != 2 || row.CostNanos != 105_000 || row.Unpriced != 1 {
				t.Errorf("otlp summary: requests=%d cost=%d unpriced=%d", row.Requests, row.CostNanos, row.Unpriced)
			}
			if row.Tokens["audio_input"] != 40 || row.Tokens["audio_output"] != 16 || row.Tokens["input"] != 186 || row.Tokens["reasoning"] != 128 {
				t.Errorf("otlp summary tokens: %+v", row.Tokens)
			}
		case "pipeline":
			// A priced cost of zero is not unpriced.
			if row.Requests != 1 || row.CostNanos != 0 || row.Unpriced != 0 {
				t.Errorf("pipeline summary: requests=%d cost=%d unpriced=%d", row.Requests, row.CostNanos, row.Unpriced)
			}
		default:
			t.Errorf("unexpected source group %+v", row.Group)
		}
	}
}
