package otlpreceiver_test

import (
	"bytes"
	"encoding/json"
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

func TestEveryAcceptedExportAnswersWhetherContentIsStored(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name                           string
		opts                           fixtureOptions
		receiverOff, payloadLoggingOff bool
		want                           string
	}{
		{name: "policy stores", opts: fixtureOptions{policyCaptures: &yes}, want: httpheader.ContentCaptureStore},
		{name: "policy does not store", opts: fixtureOptions{policyCaptures: &no}, want: httpheader.ContentCaptureDrop},
		{name: "policy does not store, key flag set", opts: fixtureOptions{policyCaptures: &no, keyCaptures: true}, want: httpheader.ContentCaptureDrop},
		{name: "no policy", want: httpheader.ContentCaptureClient},
		{name: "receiver switch off", opts: fixtureOptions{policyCaptures: &yes}, receiverOff: true, want: httpheader.ContentCaptureDrop},
		{name: "payload logging off", payloadLoggingOff: true, want: httpheader.ContentCaptureDrop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixtureWith(t, tc.opts)
			fx.captureContent = !tc.receiverOff
			fx.payloads.enabled = !tc.payloadLoggingOff

			// An empty logs export is how a client asks before it has calls to send.
			rec := fx.postLogs(t, otlp.MediaTypeJSON, []byte(`{"resourceLogs":[]}`))
			if rec.Code != http.StatusOK || rec.Header().Get(httpheader.HeaderContentCapture) != tc.want {
				t.Errorf("empty logs export: status %d %s = %q, want 200 and %q", rec.Code, httpheader.HeaderContentCapture, rec.Header().Get(httpheader.HeaderContentCapture), tc.want)
			}
			rec = fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large")))
			if rec.Code != http.StatusOK || rec.Header().Get(httpheader.HeaderContentCapture) != tc.want {
				t.Errorf("trace export: status %d %s = %q, want 200 and %q", rec.Code, httpheader.HeaderContentCapture, rec.Header().Get(httpheader.HeaderContentCapture), tc.want)
			}
		})
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

func TestTheReportedHostPricesTheCall(t *testing.T) {
	record := func(t *testing.T, provider, modelName, host string) (pricing string, cost int64) {
		t.Helper()
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1,
			str("gen_ai.operation.name", "chat"),
			str("gen_ai.provider.name", provider),
			str("gen_ai.request.model", modelName),
			str("wyolet.relay.host", host),
			num("gen_ai.usage.input_tokens", 1000),
			num("gen_ai.usage.output_tokens", 300),
		)))
		if rec.Code != http.StatusOK || len(fx.events) != 1 || fx.events[0].CostNanos == nil {
			t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
		}
		return fx.events[0].Pricing, *fx.events[0].CostNanos
	}

	if pricing, cost := record(t, "acme", "acme-large", "a-reseller"); pricing != "reseller-markup" || cost != 75_000_000 {
		t.Errorf("reported reseller: pricing %q cost %d, want reseller-markup / 75000000", pricing, cost)
	}
	// The reported host goes before a provider hint's host.
	if pricing, _ := record(t, "acme.cloud", "acme-large", "acme"); pricing != "acme-large-list" {
		t.Errorf("reported own host over the hinted reseller: pricing %q", pricing)
	}
	if pricing, _ := record(t, "acme", "acme-large", "no-such-host"); pricing != "acme-large-list" {
		t.Errorf("unknown host: pricing %q, want the provider's own host", pricing)
	}
	if pricing, _ := record(t, "acme.cloud", "acme-large", "no-such-host"); pricing != "reseller-markup" {
		t.Errorf("unknown host with a provider hint: pricing %q, want the hinted host", pricing)
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

	// (c) A split body rebuilds to the stored bytes.
	for i, s := range []dedup.Split{one, two} {
		pieces := map[dedup.Hash][]byte{}
		for _, p := range s.Pieces {
			pieces[p.Hash] = p.Body
		}
		got, err := dedup.Rebuild(s.Skeleton, s.Fields, func(h dedup.Hash) ([]byte, bool) { b, ok := pieces[h]; return b, ok })
		if want := [][]byte{turnOne, turnTwo}[i]; err != nil || !bytes.Equal(got, want) {
			t.Errorf("turn %d rebuilt = %s (%v)", i+1, got, err)
		}
	}

	whole := len(turnOne) + len(turnTwo)
	deduplicated := len(one.Skeleton) + len(two.Skeleton)
	for _, n := range distinct {
		deduplicated += n
	}
	t.Logf("two turns: %d bytes stored whole; %d bytes as %d skeleton + %d bytes in %d distinct pieces (%d piece references)",
		whole, deduplicated, len(one.Skeleton)+len(two.Skeleton), deduplicated-len(one.Skeleton)-len(two.Skeleton), len(distinct), len(one.Pieces)+len(two.Pieces))

	// The ClickHouse store's integration test stores these same bodies; the files keep the two in step.
	for name, body := range map[string][]byte{"reported_turn1_request.json": turnOne, "reported_turn2_request.json": turnTwo} {
		want, err := os.ReadFile("../../pkg/payload/clickhouse/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
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
