package otlpreceiver_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

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
