package otlpreceiver_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/usagelog"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	"net/http"
	"slices"
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
	return must(proto.Marshal(logsData(records...)))(t)
}

// logsJSON renders records as an OTLP/JSON export, which differs from protobuf JSON only in writing ids as hex.
func logsJSON(t *testing.T, records ...*logspb.LogRecord) []byte {
	t.Helper()
	b := must(protojson.Marshal(logsData(records...)))(t)
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
	return must(json.Marshal(doc))(t)
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
	// The same row the span of this call gives, minus the duration a log record cannot carry.
	want := fx.chatRow(1)
	want.DurationMs = 0
	if diff := cmp.Diff([]usagelog.Event{want}, fx.events); diff != "" {
		t.Fatalf("recorded events (-want +got):\n%s", diff)
	}
}

func TestEventOfAStreamedCallAsAnSDKExportsIt(t *testing.T) {
	fx := newFixture(t)
	// The shape SDK exporters give the event: no time_unix_nano, only the observed time; no body; the first-chunk delay but, being a log record, no duration.
	observed := spanStart.Add(120 * time.Millisecond)
	event := &logspb.LogRecord{
		TraceId:              bytes.Repeat([]byte{0xab}, 16),
		SpanId:               bytes.Repeat([]byte{1}, 8),
		EventName:            inferenceEvent,
		ObservedTimeUnixNano: uint64(observed.UnixNano()),
		Attributes: []*commonpb.KeyValue{
			str("gen_ai.operation.name", "generate_content"),
			str("gen_ai.provider.name", "acme"),
			str("gen_ai.request.model", "acme-large"),
			str("gen_ai.response.model", "acme-large"),
			str("gen_ai.response.id", "resp_stream_1"),
			{Key: "gen_ai.request.stream", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
			{Key: "gen_ai.response.time_to_first_chunk", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.0010174999479204416}}},
			{Key: "gen_ai.response.finish_reasons", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "stop"}}}}}}},
			num("gen_ai.usage.input_tokens", 800),
			num("gen_ai.usage.output_tokens", 214),
		},
	}
	if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, event)); rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d body = %s", rec.Code, len(fx.events), rec.Body)
	}
	// Dated at the observed time, with the first chunk at 1017µs and, lacking a duration, the response ending there.
	want := fx.chatRow(1)
	want.Timestamp, want.DurationMs, want.Streamed, want.FinishReason = observed, 0, true, "stop"
	want.Upstream = &sdkusage.UpstreamTiming{ResponseStart: 1017, ResponseEnd: 1017}
	want.Tokens = map[string]int64{"input": 800, "output": 214}
	cost := int64(5_610_000)
	want.CostNanos, want.CostBreakdown = &cost, map[string]int64{"tokens.input": 2_400_000, "tokens.output": 3_210_000}
	want.Extras["operation"], want.Extras["response_id"] = "generate_content", "resp_stream_1"
	delete(want.Extras, "session_id")
	if diff := cmp.Diff([]usagelog.Event{want}, fx.events); diff != "" {
		t.Fatalf("recorded events (-want +got):\n%s", diff)
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
	if got, want := rec.Body.String(), `{"partialSuccess":{"errorMessage":"model-call log records rejected: 1 with neither span ids nor a response id","rejectedLogRecords":"1"}}`; got != want {
		t.Errorf("body:\n got %s\nwant %s", got, want)
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
	rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, headless))
	if got, want := rec.Body.String(), `{"partialSuccess":{"errorMessage":"model-call spans rejected: 1 without a trace id or span id","rejectedSpans":"1"}}`; rec.Code != http.StatusOK || got != want || len(fx.events) != 2 {
		t.Errorf("span without ids: status = %d events = %d body:\n got %s\nwant %s", rec.Code, len(fx.events), got, want)
	}
}

func TestLogsShareTheLimitsOfTraces(t *testing.T) {
	t.Run("usage queue at half", func(t *testing.T) {
		fx := newFixture(t)
		fx.capacity = 2
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(1, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(2, "acme-large")))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || len(fx.events) != 1 {
			t.Errorf("status = %d Retry-After = %q events = %d, want 503, 1 and only the first event", rec.Code, rec.Header().Get("Retry-After"), len(fx.events))
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
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, chatEvent(3, "acme-large"))); rec.Code != http.StatusTooManyRequests || len(fx.events) != 2 {
			t.Errorf("third export: status = %d events = %d, want 429 and the two recorded before it", rec.Code, len(fx.events))
		}
	})
	t.Run("event starting in the future", func(t *testing.T) {
		fx := newFixture(t)
		late := chatEvent(1, "acme-large")
		late.TimeUnixNano = uint64(time.Now().Add(10 * time.Minute).UnixNano())
		rec := fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, late))
		if got, want := rec.Body.String(), `{"partialSuccess":{"errorMessage":"model-call log records rejected: 1 starting more than 5 minutes in the future","rejectedLogRecords":"1"}}`; rec.Code != http.StatusOK || len(fx.events) != 0 || got != want {
			t.Errorf("status = %d events = %d body:\n got %s\nwant %s", rec.Code, len(fx.events), got, want)
		}
	})
}
