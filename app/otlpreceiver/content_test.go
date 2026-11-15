package otlpreceiver_test

import (
	"bytes"
	"encoding/json"
	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/usagelog"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
		if n, err := x.Int64(); err == nil {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
		}
		f, err := x.Float64()
		if err != nil {
			t.Fatalf("number %q: %v", x, err)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
	case nil:
		return &commonpb.AnyValue{}
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

// One JSON object per side, keyed by the convention's attribute names, with keys sorted and nothing escaped beyond what JSON needs.
const (
	storedRequest = `{"gen_ai.input.messages":[{"parts":[{"content":"What is 2+2?","type":"text"}],"role":"user"}],` +
		`"gen_ai.system_instructions":[{"content":"Answer in <one> sentence & no more.","type":"text"}],` +
		`"gen_ai.tool.definitions":[{"name":"add","parameters":{"max":9007199254740993,"properties":{"a":{"type":"integer"}}},"type":"function"}]}`
	storedResponse = `{"gen_ai.output.messages":[{"finish_reason":"stop","parts":[{"content":"4","type":"text"}],"role":"assistant"}]}`
)

// storedContent is the payload record of the call chatRow(spanID) describes, reported with contentAttrs.
func (fx *fixture) storedContent(spanID byte) payload.Record {
	ev := fx.chatRow(spanID)
	// The log read path joins the two by request id and refuses a body whose owner differs from the event's.
	return payload.Record{
		RequestID: ev.RequestID, Timestamp: ev.Timestamp,
		ProjectID: ev.ProjectID, PrincipalID: ev.PrincipalID, RelayKeyHash: ev.RelayKeyHash,
		RequestBody: []byte(storedRequest), ResponseBody: []byte(storedResponse),
	}
}

// A span carries content as JSON text, an event as structured values; either way the same bytes are stored, keyed like the usage row.
func TestContentIsStoredAsThePayloadOfTheUsageEvent(t *testing.T) {
	for name, send := range map[string]func(fx *fixture) *httptest.ResponseRecorder{
		"span": func(fx *fixture) *httptest.ResponseRecorder {
			return fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...)))
		},
		"protobuf event": func(fx *fixture) *httptest.ResponseRecorder {
			return fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, pbEvent(1, contentAttrs(t, asStructure)...)))
		},
		"json event": func(fx *fixture) *httptest.ResponseRecorder {
			return fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, pbEvent(1, contentAttrs(t, asStructure)...)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := capturing(t)
			if rec := send(fx); rec.Code != http.StatusOK || strings.Trim(rec.Body.String(), "{}") != "" {
				t.Fatalf("status = %d body = %q, want plain success", rec.Code, rec.Body)
			}
			row := fx.chatRow(1)
			if name != "span" {
				row.DurationMs = 0
			}
			if diff := cmp.Diff([]usagelog.Event{row}, fx.events); diff != "" {
				t.Errorf("usage events (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]payload.Record{fx.storedContent(1)}, fx.payloads.records); diff != "" {
				t.Errorf("payload records (-want +got):\n%s", diff)
			}
		})
	}
}

func TestContentNeedsBothSwitches(t *testing.T) {
	withContent := pbSpan(1, contentAttrs(t, asText)...)
	for _, tc := range []struct {
		name                    string
		receiver, payloadLogger bool
		span                    *tracepb.Span
		wantBody                string
	}{
		// The usage row is recorded either way, and the client is told its content was not.
		{"receiver setting off", false, true, withContent, `{"partialSuccess":{"errorMessage":"` + captureOffWarning + `"}}`},
		{"payload logging off", true, false, withContent, `{"partialSuccess":{"errorMessage":"` + captureOffWarning + `"}}`},
		{"a call without content stores nothing", true, true, chatSpan(1, "acme-large"), "{}"},
		{"the earlier content attributes are not read", true, true, pbSpan(1, append(chatAttrs("acme-large"), str("gen_ai.prompt", "What is 2+2?"), str("gen_ai.completion", "4"))...), "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.captureContent, fx.payloads.enabled = tc.receiver, tc.payloadLogger
			rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, tc.span))
			if got := rec.Body.String(); rec.Code != http.StatusOK || got != tc.wantBody {
				t.Errorf("status = %d body:\n got %s\nwant %s", rec.Code, got, tc.wantBody)
			}
			want := fx.chatRow(1)
			delete(want.Extras, "service") // mustJSON's export names no resource
			if diff := cmp.Diff([]usagelog.Event{want}, fx.events); diff != "" {
				t.Errorf("usage events (-want +got):\n%s", diff)
			}
			if len(fx.payloads.records) != 0 {
				t.Errorf("stored %d payload records, want none", len(fx.payloads.records))
			}
		})
	}
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
	if diff := cmp.Diff([]usagelog.Event{fx.chatRow(1)}, fx.events); diff != "" {
		t.Errorf("usage events, want the span's one row (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]payload.Record{fx.storedContent(1)}, fx.payloads.records); diff != "" {
		t.Errorf("payload records, want the content once (-want +got):\n%s", diff)
	}
}

func TestContentIsCutToThePayloadCap(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxBytes    int
		request     string
		requestCut  bool
		response    string
		responseCut bool
	}{
		{"request over, response under", 150, storedRequest[:150], true, storedResponse, false},
		{"both over", 40, storedRequest[:40], true, storedResponse[:40], true},
		{"cap at the request's exact size", len(storedRequest), storedRequest, false, storedResponse, false},
		{"no cap", 0, storedRequest, false, storedResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := capturing(t)
			fx.payloads.maxBytes = tc.maxBytes
			if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, contentAttrs(t, asText)...))); rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			want := fx.storedContent(1)
			want.RequestBody, want.RequestTruncated = []byte(tc.request), tc.requestCut
			want.ResponseBody, want.ResponseTruncated = []byte(tc.response), tc.responseCut
			if diff := cmp.Diff([]payload.Record{want}, fx.payloads.records); diff != "" {
				t.Errorf("payload records (-want +got):\n%s", diff)
			}
		})
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
	if diff := cmp.Diff([]payload.Record{fx.storedContent(1)}, fx.payloads.records); diff != "" || len(fx.events) != 1 {
		t.Errorf("after the resend: %d events, payload records (-want +got):\n%s", len(fx.events), diff)
	}
}

func TestContentOfSeveralCallsStopsAtHalfThePayloadQueue(t *testing.T) {
	fx := capturing(t)
	fx.payloads.capacity = 4
	body := export(t, pbSpan(1, contentAttrs(t, asText)...), pbSpan(2, contentAttrs(t, asText)...), pbSpan(3, contentAttrs(t, asText)...))

	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK || len(fx.events) != 3 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	if diff := cmp.Diff([]payload.Record{fx.storedContent(1), fx.storedContent(2)}, fx.payloads.records); diff != "" {
		t.Fatalf("payload records, want the receiver to stop at half of 4 (-want +got):\n%s", diff)
	}
	// The call that did not fit was unmarked: the resend stores exactly that one.
	fx.payloads.records = nil
	fx.post(t, otlp.MediaTypeProtobuf, body)
	if diff := cmp.Diff([]payload.Record{fx.storedContent(3)}, fx.payloads.records); diff != "" || len(fx.events) != 3 {
		t.Errorf("resend: %d events, payload records (-want +got):\n%s", len(fx.events), diff)
	}
}

func TestARefusedExportCarriesNoAnswer(t *testing.T) {
	fx := newFixture(t)
	fx.capacity = 0
	rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large")))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(httpheader.HeaderContentCapture) != "" {
		t.Errorf("status %d answer %q, want 503 without an answer", rec.Code, rec.Header().Get(httpheader.HeaderContentCapture))
	}
}

// Two consecutive turns of one conversation, as the JSON text exporters put on spans: spaces after separators, keys in the exporter's order, and floats written with a fraction. The second turn resends the first turn's message and adds two.
const (
	turnInstructions = `[{"type": "text", "content": "You are a travel assistant. Answer in one short paragraph, name the source of every figure you give, and call a tool rather than guess whenever the question depends on current conditions."}]`
	turnTools        = `[{"type": "function", "name": "get_weather", "description": "Current conditions and a three day forecast for a city.", "parameters": {"type": "object", "properties": {"city": {"type": "string"}, "days": {"type": "integer", "default": 3}, "min_confidence": {"type": "number", "default": 0.50}}, "required": ["city"]}}, ` +
		`{"type": "function", "name": "find_flights", "description": "Direct flights between two airports on a date, cheapest first.", "parameters": {"type": "object", "properties": {"from": {"type": "string"}, "to": {"type": "string"}, "date": {"type": "string"}, "max_price": {"type": "number", "default": 1.0e3}}, "required": ["from", "to", "date"]}}]`
	turnOneMessage  = `{"role": "user", "name": null, "parts": [{"type": "text", "content": "I am flying to Tashkent on Friday. What will the weather be like, and is it <warmer> than Samarkand & Bukhara?"}]}`
	turnTwoMessages = `{"role": "assistant", "parts": [{"type": "tool_call", "id": "call_1", "name": "get_weather", "arguments": {"city": "Tashkent", "days": 3, "min_confidence": 1.0}}]}, ` +
		`{"role": "tool", "parts": [{"type": "tool_call_response", "id": "call_1", "response": {"today": {"high": 31.5, "low": 18, "sky": "clear"}, "source": "met office"}}]}`
	turnOneOutput = `[{"role": "assistant", "finish_reason": "tool_calls", "parts": [{"type": "tool_call", "id": "call_1", "name": "get_weather", "arguments": {"city": "Tashkent", "days": 3, "min_confidence": 1.0}}]}]`
	turnTwoOutput = `[{"role": "assistant", "finish_reason": "stop", "parts": [{"type": "text", "content": "Friday in Tashkent will be clear, 31.5 degrees at most (met office)."}]}]`
)

func turnAttrs(t *testing.T, form func(*testing.T, string, string) *commonpb.KeyValue, messages, output string) []*commonpb.KeyValue {
	t.Helper()
	return append(chatAttrs("acme-large"),
		form(t, "gen_ai.system_instructions", turnInstructions),
		form(t, "gen_ai.tool.definitions", turnTools),
		form(t, "gen_ai.input.messages", "["+messages+"]"),
		form(t, "gen_ai.output.messages", output),
	)
}

// asSpan and asEvent are the two ways a call's content is reported: JSON text on a span, structured values on an event.
const (
	asSpan  = false
	asEvent = true
)

// storedTurns reports the two turns as two calls, each as a span or as an event, and returns the request bodies the receiver stored for them.
func storedTurns(t *testing.T, first, second bool) (turnOne, turnTwo []byte) {
	t.Helper()
	fx := capturing(t)
	send := func(spanID byte, event bool, messages, output string) {
		var rec *httptest.ResponseRecorder
		if event {
			rec = fx.postLogs(t, otlp.MediaTypeProtobuf, logsExport(t, pbEvent(spanID, turnAttrs(t, asStructure, messages, output)...)))
		} else {
			rec = fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(spanID, turnAttrs(t, asText, messages, output)...)))
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
		}
	}
	send(1, first, turnOneMessage, turnOneOutput)
	send(2, second, turnOneMessage+", "+turnTwoMessages, turnTwoOutput)
	if len(fx.payloads.records) != 2 {
		t.Fatalf("payload records = %d, want one per turn", len(fx.payloads.records))
	}
	return fx.payloads.records[0].RequestBody, fx.payloads.records[1].RequestBody
}

// hashesOf returns the piece hashes of one top-level array of a split body, failing when the field was not split per element.
func hashesOf(t *testing.T, s dedup.Split, field string) []dedup.Hash {
	t.Helper()
	for _, f := range s.Fields {
		if f.Name == field {
			if !f.Array {
				t.Fatalf("%s is stored as one piece, want one per element", field)
			}
			return f.Hashes
		}
	}
	t.Fatalf("%s is not a split field of the body; fields = %+v", field, s.Fields)
	return nil
}

func TestStoredContentSplitsIntoSharedPieces(t *testing.T) {
	// The first turn arrives on a span, the second on an event: the hardest case for sharing, since the two forms reach the receiver as different data.
	turnOne, turnTwo := storedTurns(t, asSpan, asEvent)
	one, two := dedup.SplitBody(turnOne), dedup.SplitBody(turnTwo)
	if one.Whole() || two.Whole() {
		t.Fatalf("a body was stored whole:\n%s\n%s", turnOne, turnTwo)
	}

	// (a) One piece per message, per tool definition and per instruction part.
	for field, want := range map[string][2]int{
		"gen_ai.input.messages":      {1, 3},
		"gen_ai.tool.definitions":    {2, 2},
		"gen_ai.system_instructions": {1, 1},
	} {
		if got := [2]int{len(hashesOf(t, one, field)), len(hashesOf(t, two, field))}; got != want {
			t.Errorf("%s pieces per turn = %v, want %v", field, got, want)
		}
	}

	// (b) Everything the second turn repeats is the same piece: its messages start with the first turn's, and the tools and instructions are unchanged.
	if shared := dedup.CommonPrefix(hashesOf(t, one, "gen_ai.input.messages"), hashesOf(t, two, "gen_ai.input.messages")); shared != 1 {
		t.Errorf("the turns share %d leading messages, want 1", shared)
	}
	for _, field := range []string{"gen_ai.tool.definitions", "gen_ai.system_instructions"} {
		a, b := hashesOf(t, one, field), hashesOf(t, two, field)
		if dedup.CommonPrefix(a, b) != len(a) || len(a) != len(b) {
			t.Errorf("%s pieces differ between the turns", field)
		}
	}
	distinct := map[dedup.Hash]int{}
	for _, s := range []dedup.Split{one, two} {
		for _, p := range s.Pieces {
			distinct[p.Hash] = len(p.Body)
		}
	}
	if len(one.Pieces) != 4 || len(two.Pieces) != 6 || len(distinct) != 6 {
		t.Errorf("pieces = %d and %d, %d distinct; want 4 and 6, 6 distinct", len(one.Pieces), len(two.Pieces), len(distinct))
	}

	// The ClickHouse store's integration test stores these same bodies; the files keep the two in step.
	for name, body := range map[string][]byte{"reported_turn1_request.json": turnOne, "reported_turn2_request.json": turnTwo} {
		want := must(os.ReadFile("../../pkg/payload/clickhouse/testdata/" + name))(t)
		if !bytes.Equal(bytes.TrimSuffix(want, []byte("\n")), body) {
			t.Errorf("%s no longer holds what the receiver stores:\n%s", name, body)
		}
	}
}

func TestStoredContentIsTheSameBytesInEveryForm(t *testing.T) {
	textOne, textTwo := storedTurns(t, asSpan, asSpan)
	for name, forms := range map[string][2]bool{
		"events":          {asEvent, asEvent},
		"span then event": {asSpan, asEvent},
		"event then span": {asEvent, asSpan},
	} {
		one, two := storedTurns(t, forms[0], forms[1])
		if !bytes.Equal(one, textOne) || !bytes.Equal(two, textTwo) {
			t.Errorf("%s stored\n%s\n%s\nspans stored\n%s\n%s", name, one, two, textOne, textTwo)
		}
	}
}
