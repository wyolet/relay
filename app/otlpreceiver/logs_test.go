package otlpreceiver_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/wyolet/relay/pkg/otlp"
)

const inferenceEvent = "gen_ai.client.inference.operation.details"

// pbEvent is an inference details event emitted inside the span pbSpan builds for the same id.
func pbEvent(spanID byte, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TraceId:      bytes.Repeat([]byte{0xab}, 16),
		SpanId:       bytes.Repeat([]byte{spanID}, 8),
		EventName:    inferenceEvent,
		TimeUnixNano: uint64(spanStart.UnixNano()),
		Attributes:   attrs,
	}
}

func chatEvent(spanID byte, modelName string) *logspb.LogRecord {
	return pbEvent(spanID, chatAttrs(modelName)...)
}

// detachedEvent is a chat event emitted outside any span, known only by the provider's response id.
func detachedEvent(responseID string) *logspb.LogRecord {
	return &logspb.LogRecord{
		EventName:    inferenceEvent,
		TimeUnixNano: uint64(spanStart.UnixNano()),
		Attributes: []*commonpb.KeyValue{
			str("gen_ai.operation.name", "chat"),
			str("gen_ai.request.model", "acme-large"),
			str("gen_ai.response.id", responseID),
			num("gen_ai.usage.input_tokens", 10),
			num("gen_ai.usage.output_tokens", 5),
		},
	}
}

func logsData(records ...*logspb.LogRecord) *logspb.LogsData {
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "billing-agent")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}
}

func logsExport(t *testing.T, records ...*logspb.LogRecord) []byte {
	t.Helper()
	b, err := proto.Marshal(logsData(records...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// logsJSON renders records as an OTLP/JSON export, which differs from protobuf JSON only in writing ids as hex.
func logsJSON(t *testing.T, records ...*logspb.LogRecord) []byte {
	t.Helper()
	b, err := protojson.Marshal(logsData(records...))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for i, r := range records {
		rec := doc.(map[string]any)["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[i].(map[string]any)
		delete(rec, "traceId")
		delete(rec, "spanId")
		if len(r.TraceId) > 0 {
			rec["traceId"] = hex.EncodeToString(r.TraceId)
		}
		if len(r.SpanId) > 0 {
			rec["spanId"] = hex.EncodeToString(r.SpanId)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func responseRequestID(responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return "otlp-" + hex.EncodeToString(sum[:16]) + "-response"
}

func TestLogsExportRecordsAUsageEvent(t *testing.T) {
	fx := newFixture(t)
	rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large")))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q, want the empty success message", rec.Code, rec.Body)
	}
	if len(fx.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(fx.events))
	}
	ev := fx.events[0]
	// The same row the span of this call would give, minus the duration a log record cannot carry.
	if ev.RequestID != "otlp-"+strings.Repeat("ab", 16)+"-"+strings.Repeat("01", 8) {
		t.Errorf("RequestID = %q, want it derived from the ids of the span the event was emitted in", ev.RequestID)
	}
	if ev.Source != "otlp" || ev.Status != 200 || ev.DurationMs != 0 || !ev.Timestamp.Equal(spanStart) {
		t.Errorf("outcome = source %q status %d duration %d ts %v", ev.Source, ev.Status, ev.DurationMs, ev.Timestamp)
	}
	if ev.ProjectID != fx.project.Meta.ID || ev.RelayKeyHash != fx.keyRow.Spec.KeyHash {
		t.Errorf("attribution = project %q key %q", ev.ProjectID, ev.RelayKeyHash)
	}
	if ev.ModelID != fx.model.Meta.ID || ev.Pricing != "acme-large-list" || ev.CostNanos == nil || *ev.CostNanos != 5_760_000 {
		t.Errorf("model = %q pricing = %q cost = %v", ev.Model, ev.Pricing, ev.CostNanos)
	}
	if ev.Tokens["input"] != 100 || ev.Tokens["cache_read"] != 700 || ev.Tokens["output"] != 300 {
		t.Errorf("tokens = %v", ev.Tokens)
	}
	if ev.Extras["service"] != "billing-agent" || ev.Extras["response_id"] != "resp_123" || ev.Extras["span_id"] != strings.Repeat("01", 8) {
		t.Errorf("extras = %v", ev.Extras)
	}
}

func TestLogsExportKeepsOnlyTheModelCallEvent(t *testing.T) {
	fx := newFixture(t)
	plain := pbEvent(2, str("message", "cache warmed"))
	plain.EventName = ""
	otherEvent := pbEvent(3, chatAttrs("acme-large")...)
	otherEvent.EventName = "gen_ai.evaluation.result"
	// Exporters older than the event_name field name the event in an attribute.
	olderExporter := pbEvent(4, append(chatAttrs("acme-large"), str("event.name", inferenceEvent))...)
	olderExporter.EventName = ""
	anonymous := detachedEvent("")

	rec := fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, chatEvent(1, "acme-large"), plain, otherEvent, olderExporter, anonymous))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if got, want := fx.spanIDs(), spanIDsOf(1, 4); !slices.Equal(got, want) {
		t.Fatalf("recorded = %v, want the two model-call events %v", got, want)
	}
	var resp struct {
		PartialSuccess struct {
			RejectedLogRecords string `json:"rejectedLogRecords"`
			ErrorMessage       string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body, err)
	}
	if resp.PartialSuccess.RejectedLogRecords != "1" || !strings.Contains(resp.PartialSuccess.ErrorMessage, "response id") {
		t.Errorf("partial success = %+v, want 1 log record rejected for having no identity", resp.PartialSuccess)
	}
}

func TestSpanAndEventOfOneCallAreOneUsageRow(t *testing.T) {
	t.Run("span first", func(t *testing.T) {
		fx := newFixture(t)
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("span: status = %d", rec.Code)
		}
		rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large"), chatEvent(2, "acme-large")))
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("event: status = %d body = %q, want plain success", rec.Code, rec.Body)
		}
		if got, want := fx.spanIDs(), spanIDsOf(1, 2); !slices.Equal(got, want) {
			t.Fatalf("recorded = %v, want %v", got, want)
		}
		if fx.events[0].DurationMs != 2000 {
			t.Errorf("duration = %d, want the span's, which arrived first", fx.events[0].DurationMs)
		}
	})
	t.Run("event first", func(t *testing.T) {
		fx := newFixture(t)
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("event: status = %d", rec.Code)
		}
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("span: status = %d", rec.Code)
		}
		if len(fx.events) != 1 {
			t.Fatalf("recorded %d events, want 1", len(fx.events))
		}
	})
}

func TestEventOutsideASpanIsKnownByItsResponseID(t *testing.T) {
	fx := newFixture(t)
	body := logsExport(t, detachedEvent("resp_a"), detachedEvent("resp_b"), detachedEvent("resp_a"))
	for range 2 {
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("status = %d body = %q, want plain success", rec.Code, rec.Body)
		}
	}
	if len(fx.events) != 2 {
		t.Fatalf("recorded %d events, want one per response id", len(fx.events))
	}
	ids := []string{fx.events[0].RequestID, fx.events[1].RequestID}
	if want := []string{responseRequestID("resp_a"), responseRequestID("resp_b")}; !slices.Equal(ids, want) {
		t.Errorf("request ids = %v, want %v", ids, want)
	}
	ev := fx.events[0]
	if _, ok := ev.Extras["trace_id"]; ok || ev.Extras["response_id"] != "resp_a" {
		t.Errorf("extras = %v, want the response id and no span ids", ev.Extras)
	}

	// A span has ids of its own; one without them is malformed and is not rescued by its response id.
	headless := chatSpan(9, "acme-large")
	headless.SpanId = nil
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, headless)); rec.Code != http.StatusOK || len(fx.events) != 2 {
		t.Errorf("span without ids: status = %d events = %d, want it rejected", rec.Code, len(fx.events))
	}
}

func TestLogsShareTheLimitsOfTraces(t *testing.T) {
	t.Run("receiver disabled", func(t *testing.T) {
		fx := newFixture(t)
		fx.enabled = false
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large"))); rec.Code != http.StatusNotFound || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
	t.Run("no credential", func(t *testing.T) {
		fx := newFixture(t)
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large")), "Authorization", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("undecodable body", func(t *testing.T) {
		fx := newFixture(t)
		if rec := fx.postLogs(t, otlp.MediaTypeJSON, []byte(`{"resourceLogs":`)); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("usage queue at half", func(t *testing.T) {
		fx := newFixture(t)
		fx.capacity = 2
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(2, "acme-large")))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("status = %d Retry-After = %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
		}
	})
	t.Run("one export budget for both signals", func(t *testing.T) {
		fx := newFixtureWith(t, fixtureOptions{exportsPerMinute: 2})
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("traces: status = %d", rec.Code)
		}
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(2, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("logs: status = %d", rec.Code)
		}
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(3, "acme-large"))); rec.Code != http.StatusTooManyRequests {
			t.Errorf("third export: status = %d, want 429", rec.Code)
		}
	})
	t.Run("event starting in the future", func(t *testing.T) {
		fx := newFixture(t)
		late := chatEvent(1, "acme-large")
		late.TimeUnixNano = uint64(time.Now().Add(10 * time.Minute).UnixNano())
		rec := fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, late))
		if rec.Code != http.StatusOK || len(fx.events) != 0 || !strings.Contains(rec.Body.String(), "rejectedLogRecords") {
			t.Errorf("status = %d events = %d body = %s, want it rejected in a partial success", rec.Code, len(fx.events), rec.Body)
		}
	})
}
