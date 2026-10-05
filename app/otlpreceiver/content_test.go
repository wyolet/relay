package otlpreceiver_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/payload"
)

// The content of one call, as the JSON text exporters put on spans.
const (
	instructionsJSON = `[{"type": "text", "content": "Answer in <one> sentence & no more."}]`
	inputJSON        = `[{"role": "user", "parts": [{"type": "text", "content": "What is 2+2?"}]}]`
	toolsJSON        = `[{"type": "function", "name": "add", "parameters": {"properties": {"a": {"type": "integer"}}, "max": 9007199254740993}}]`
	outputJSON       = `[{"role": "assistant", "finish_reason": "stop", "parts": [{"type": "text", "content": "4"}]}]`
)

// structured converts decoded JSON into the OTLP value an exporter sends for the same data on an event.
func structured(t *testing.T, v any) *commonpb.AnyValue {
	t.Helper()
	switch x := v.(type) {
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			t.Fatalf("number %q is not an integer", x)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
	case []any:
		out := make([]*commonpb.AnyValue, len(x))
		for i, e := range x {
			out[i] = structured(t, e)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: out}}}
	case map[string]any:
		var out []*commonpb.KeyValue
		for k, e := range x {
			out = append(out, &commonpb.KeyValue{Key: k, Value: structured(t, e)})
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: out}}}
	}
	t.Fatalf("no OTLP value for %T", v)
	return nil
}

// asText and asStructure are the two ways one content attribute travels.
func asText(_ *testing.T, name, text string) *commonpb.KeyValue { return str(name, text) }

func asStructure(t *testing.T, name, text string) *commonpb.KeyValue {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return &commonpb.KeyValue{Key: name, Value: structured(t, v)}
}

func contentAttrs(t *testing.T, form func(*testing.T, string, string) *commonpb.KeyValue) []*commonpb.KeyValue {
	t.Helper()
	return append(chatAttrs("acme-large"),
		form(t, "gen_ai.system_instructions", instructionsJSON),
		form(t, "gen_ai.input.messages", inputJSON),
		form(t, "gen_ai.tool.definitions", toolsJSON),
		form(t, "gen_ai.output.messages", outputJSON),
	)
}

// capturing returns a fixture with both content gates open.
func capturing(t *testing.T) *fixture {
	t.Helper()
	fx := newFixture(t)
	fx.captureContent = true
	fx.payloads.enabled = true
	return fx
}

func TestContentIsStoredAsThePayloadOfTheUsageEvent(t *testing.T) {
	fx := capturing(t)
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if len(fx.events) != 1 || len(fx.payloads.records) != 1 {
		t.Fatalf("events = %d payload records = %d, want 1 and 1", len(fx.events), len(fx.payloads.records))
	}
	ev, rec := fx.events[0], fx.payloads.records[0]
	// The log read path joins the two by request id and refuses a body whose owner differs from the event's.
	if rec.RequestID != ev.RequestID || !rec.Timestamp.Equal(ev.Timestamp) {
		t.Errorf("record keyed %q at %v, event %q at %v", rec.RequestID, rec.Timestamp, ev.RequestID, ev.Timestamp)
	}
	if rec.ProjectID != ev.ProjectID || rec.PrincipalID != ev.PrincipalID || rec.RelayKeyHash != ev.RelayKeyHash || rec.ProjectID == "" || rec.RelayKeyHash == "" {
		t.Errorf("record owner = %q %q %q, event owner = %q %q %q", rec.ProjectID, rec.PrincipalID, rec.RelayKeyHash, ev.ProjectID, ev.PrincipalID, ev.RelayKeyHash)
	}

	// One JSON object per side, keyed by the convention's attribute names, with keys sorted and nothing escaped beyond what JSON needs.
	wantRequest := `{"gen_ai.input.messages":[{"parts":[{"content":"What is 2+2?","type":"text"}],"role":"user"}],` +
		`"gen_ai.system_instructions":[{"content":"Answer in <one> sentence & no more.","type":"text"}],` +
		`"gen_ai.tool.definitions":[{"name":"add","parameters":{"max":9007199254740993,"properties":{"a":{"type":"integer"}}},"type":"function"}]}`
	wantResponse := `{"gen_ai.output.messages":[{"finish_reason":"stop","parts":[{"content":"4","type":"text"}],"role":"assistant"}]}`
	if string(rec.RequestBody) != wantRequest {
		t.Errorf("request body =\n%s\nwant\n%s", rec.RequestBody, wantRequest)
	}
	if string(rec.ResponseBody) != wantResponse {
		t.Errorf("response body =\n%s\nwant\n%s", rec.ResponseBody, wantResponse)
	}
	if rec.RequestTruncated || rec.ResponseTruncated {
		t.Errorf("truncation flags set on bodies under the cap")
	}
}

func TestContentStoresTheSameBytesFromASpanAndAnEvent(t *testing.T) {
	fromSpan := capturing(t)
	fromSpan.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...)))
	fromEvent := capturing(t)
	fromEvent.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, pbEvent(1, contentAttrs(t, asStructure)...)))
	fromJSONEvent := capturing(t)
	fromJSONEvent.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, pbEvent(1, contentAttrs(t, asStructure)...)))

	if len(fromSpan.payloads.records) != 1 || len(fromEvent.payloads.records) != 1 || len(fromJSONEvent.payloads.records) != 1 {
		t.Fatalf("payload records = %d / %d / %d, want 1 each", len(fromSpan.payloads.records), len(fromEvent.payloads.records), len(fromJSONEvent.payloads.records))
	}
	want := fromSpan.payloads.records[0]
	for name, fx := range map[string]*fixture{"protobuf event": fromEvent, "json event": fromJSONEvent} {
		got := fx.payloads.records[0]
		if got.RequestID != want.RequestID || string(got.RequestBody) != string(want.RequestBody) || string(got.ResponseBody) != string(want.ResponseBody) {
			t.Errorf("%s stored\n%s\n%s\nthe span stored\n%s\n%s", name, got.RequestBody, got.ResponseBody, want.RequestBody, want.ResponseBody)
		}
	}
}

func TestContentNeedsBothSwitches(t *testing.T) {
	body := func(t *testing.T) []byte { return export(t, pbSpan(1, contentAttrs(t, asText)...)) }

	t.Run("receiver setting off", func(t *testing.T) {
		fx := newFixture(t)
		fx.payloads.enabled = true
		if rec := fx.post(t, otlp.MediaTypeProtobuf, body(t)); rec.Code != http.StatusOK || len(fx.events) != 1 || len(fx.payloads.records) != 0 {
			t.Errorf("status = %d events = %d payload records = %d, want the usage row and no content", rec.Code, len(fx.events), len(fx.payloads.records))
		}
	})
	t.Run("payload logging off", func(t *testing.T) {
		fx := newFixture(t)
		fx.captureContent = true
		if rec := fx.post(t, otlp.MediaTypeProtobuf, body(t)); rec.Code != http.StatusOK || len(fx.events) != 1 || len(fx.payloads.records) != 0 {
			t.Errorf("status = %d events = %d payload records = %d, want the usage row and no content", rec.Code, len(fx.events), len(fx.payloads.records))
		}
	})
	t.Run("a call without content stores nothing", func(t *testing.T) {
		fx := capturing(t)
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK || len(fx.payloads.records) != 0 {
			t.Errorf("status = %d payload records = %d", rec.Code, len(fx.payloads.records))
		}
	})
	t.Run("the earlier content attributes are not read", func(t *testing.T) {
		fx := capturing(t)
		old := pbSpan(1, append(chatAttrs("acme-large"), str("gen_ai.prompt", "What is 2+2?"), str("gen_ai.completion", "4"))...)
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, old)); rec.Code != http.StatusOK || len(fx.payloads.records) != 0 {
			t.Errorf("status = %d payload records = %d", rec.Code, len(fx.payloads.records))
		}
	})
}

func TestContentArrivingAfterTheUsageRowIsStoredOnce(t *testing.T) {
	fx := capturing(t)
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK {
		t.Fatalf("span: status = %d", rec.Code)
	}
	event := logsExport(t, pbEvent(1, contentAttrs(t, asStructure)...))
	for range 2 {
		if rec := fx.postLogs(t, otlp.MediaTypeProtobuf, event); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("event: status = %d body = %q, want plain success", rec.Code, rec.Body)
		}
	}
	// The span with content, sent after the event already stored it.
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...))); rec.Code != http.StatusOK {
		t.Fatalf("span with content: status = %d", rec.Code)
	}
	if len(fx.events) != 1 || len(fx.payloads.records) != 1 {
		t.Fatalf("events = %d payload records = %d, want one usage row and its content once", len(fx.events), len(fx.payloads.records))
	}
	if fx.payloads.records[0].RequestID != fx.events[0].RequestID {
		t.Errorf("content keyed %q, usage row %q", fx.payloads.records[0].RequestID, fx.events[0].RequestID)
	}
}

func TestContentIsCutToThePayloadCap(t *testing.T) {
	fx := capturing(t)
	fx.payloads.maxBytes = 150
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...))); rec.Code != http.StatusOK || len(fx.payloads.records) != 1 {
		t.Fatalf("status = %d payload records = %d", rec.Code, len(fx.payloads.records))
	}
	rec := fx.payloads.records[0]
	if len(rec.RequestBody) != 150 || !rec.RequestTruncated {
		t.Errorf("request body = %d bytes truncated %v, want 150 and flagged", len(rec.RequestBody), rec.RequestTruncated)
	}
	if len(rec.ResponseBody) >= 150 || rec.ResponseTruncated {
		t.Errorf("response body = %d bytes truncated %v, want it whole and unflagged", len(rec.ResponseBody), rec.ResponseTruncated)
	}
}

func TestContentYieldsToAFullPayloadQueue(t *testing.T) {
	fx := capturing(t)
	// Half the queue is the receiver's to use, and that half is taken.
	fx.payloads.capacity = 4
	fx.payloads.records = make([]payload.Record, 2)
	body := export(t, pbSpan(1, contentAttrs(t, asText)...))

	rec := fx.post(t, otlp.MediaTypeProtobuf, body)
	// Content is secondary: the usage row is recorded and the export succeeds without it.
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(fx.events) != 1 {
		t.Fatalf("status = %d body = %q events = %d, want plain success and the usage row", rec.Code, rec.Body, len(fx.events))
	}
	if len(fx.payloads.records) != 2 {
		t.Fatalf("queued %d payload records into a queue at half", len(fx.payloads.records)-2)
	}

	// The content was not marked as stored, so a later report of the call still lands it.
	fx.payloads.records = nil
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK {
		t.Fatalf("resend: status = %d", rec.Code)
	}
	if len(fx.events) != 1 || len(fx.payloads.records) != 1 {
		t.Errorf("after the resend: events = %d payload records = %d, want 1 and 1", len(fx.events), len(fx.payloads.records))
	}
}

func TestContentOfSeveralCallsStopsAtHalfThePayloadQueue(t *testing.T) {
	fx := capturing(t)
	fx.payloads.capacity = 4
	body := export(t, pbSpan(1, contentAttrs(t, asText)...), pbSpan(2, contentAttrs(t, asText)...), pbSpan(3, contentAttrs(t, asText)...))

	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK || len(fx.events) != 3 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	if len(fx.payloads.records) != 2 {
		t.Fatalf("queued %d payload records, want the receiver to stop at half of 4", len(fx.payloads.records))
	}
	// The call that did not fit was unmarked: the resend stores exactly that one.
	fx.payloads.records = nil
	fx.post(t, otlp.MediaTypeProtobuf, body)
	if len(fx.payloads.records) != 1 || fx.payloads.records[0].RequestID != fx.events[2].RequestID {
		t.Errorf("resend queued %d payload records, want only the third call's", len(fx.payloads.records))
	}
}
