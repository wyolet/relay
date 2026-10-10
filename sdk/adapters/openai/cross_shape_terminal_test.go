package openai_test

// Cross-shape terminal-status tests. Each serializer must NOT emit a success
// signal when the canonical response carries a non-success status with an
// empty or other finish_reason (rule 11: no silent drops).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// --- helpers ---

func xsSSE(event, data string) []byte {
	if event == "" {
		return []byte("data: " + data + "\n\n")
	}
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

func xsSplit(b []byte) [][]byte {
	var out [][]byte
	for _, f := range strings.Split(string(b), "\n\n") {
		if strings.TrimSpace(f) != "" {
			out = append(out, []byte(f+"\n\n"))
		}
	}
	return out
}

func xsPipe(t *testing.T, toCanon, fromCanon func([]byte) ([]byte, error), frames ...[]byte) (canon, wire []byte) {
	t.Helper()
	for _, fr := range frames {
		c, err := toCanon(fr)
		if err != nil {
			t.Fatalf("toCanon: %v", err)
		}
		canon = append(canon, c...)
		for _, cf := range xsSplit(c) {
			w, err := fromCanon(cf)
			if err != nil {
				t.Fatalf("fromCanon: %v", err)
			}
			wire = append(wire, w...)
		}
	}
	return canon, wire
}

// responsesFailedStreamFrames is a Responses stream ending in response.failed.
var responsesFailedStreamFrames = [][]byte{
	xsSSE("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`),
	xsSSE("response.failed", `{"type":"response.failed","sequence_number":3,"response":{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"The model produced invalid output"}}}`),
}

// anthropicUnknownStopFrames is an Anthropic stream ending with an unrecognised stop_reason.
var anthropicUnknownStopFrames = [][]byte{
	xsSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
	xsSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	xsSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
	xsSSE("content_block_stop", `{"type":"content_block_stop","index":0}`),
	xsSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"model_context_window_exceeded","stop_sequence":null},"usage":{"output_tokens":5}}`),
	xsSSE("message_stop", `{"type":"message_stop"}`),
}

// --- CC stream tests ---

// Anthropic stream with unknown stop_reason → cross-shape to CC must not emit finish_reason=stop.
func TestCrossShape_AnthropicUnknownStop_CCStream_NotStop(t *testing.T) {
	_, wire := xsPipe(t, anthropic.AnthropicTranslator{}.NewToCanonicalStream(), openai.CCTranslator{}.NewFromCanonicalStream(), anthropicUnknownStopFrames...)
	t.Logf("CC wire:\n%s", wire)
	if strings.Contains(string(wire), `"finish_reason":"stop"`) {
		t.Fatalf("Anthropic unknown stop_reason must not produce CC finish_reason=stop; got:\n%s", wire)
	}
}

// Responses response.failed stream → cross-shape to CC must not emit finish_reason=stop.
func TestCrossShape_ResponsesFailed_CCStream_NotStop(t *testing.T) {
	_, wire := xsPipe(t, openai.ResponsesTranslator{}.NewToCanonicalStream(), openai.CCTranslator{}.NewFromCanonicalStream(), responsesFailedStreamFrames...)
	t.Logf("CC wire:\n%s", wire)
	if strings.Contains(string(wire), `"finish_reason":"stop"`) {
		t.Fatalf("response.failed must not produce CC finish_reason=stop; got:\n%s", wire)
	}
}

// --- CC buffered test ---

// Anthropic unknown stop_reason → cross-shape buffered to CC must not emit finish_reason=stop.
func TestCrossShape_AnthropicUnknownStop_CCBuffered_NotStop(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"partial"}],"stop_reason":"model_context_window_exceeded","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`)
	resp, err := anthropic.AnthropicTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther {
		t.Fatalf("precondition: expected incomplete/other, got %q/%q", resp.Status, resp.FinishReason)
	}
	cc, err := openai.CCTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CC wire: %s", cc)
	var r struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(cc, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Choices) > 0 && r.Choices[0].FinishReason == "stop" {
		t.Fatalf("Anthropic unknown stop_reason (status=incomplete, finish=other) must not produce CC finish_reason=stop; got %q", r.Choices[0].FinishReason)
	}
}

// --- Anthropic stream tests ---

// Anthropic stream with unknown stop_reason → cross-shape to Anthropic must not produce end_turn.
// (Same-vendor round-trip uses the raw reason; cross-vendor must surface a non-success signal.)
func TestCrossShape_AnthropicUnknownStop_AnthropicStream_NotEndTurn(t *testing.T) {
	// NOTE: same-vendor Anthropic→Anthropic correctly carries the raw reason;
	// this test checks the cross-vendor case via a direct canonical response.
	resp := &v1.Response{
		ID:                "r1",
		Status:            v1.StatusIncomplete,
		FinishReason:      "",
		IncompleteDetails: &v1.IncompleteDetails{Reason: "anthropic:model_context_window_exceeded"},
	}
	out, err := anthropic.AnthropicTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Anthropic wire: %s", out)
	// Anthropic serializer uses incomplete.Reason to reconstruct the raw stop_reason
	// for same-vendor round-trips — this should yield the raw vendor reason, not end_turn.
	if !strings.Contains(string(out), `"stop_reason":"model_context_window_exceeded"`) {
		t.Fatalf("Anthropic with incomplete/\"\" and vendor-prefixed reason must carry the raw stop_reason; got:\n%s", out)
	}
}

// Responses response.failed stream → cross-shape to Anthropic stream must not produce stop_reason=end_turn.
func TestCrossShape_ResponsesFailed_AnthropicStream_NotEndTurn(t *testing.T) {
	_, wire := xsPipe(t, openai.ResponsesTranslator{}.NewToCanonicalStream(), anthropic.AnthropicTranslator{}.NewFromCanonicalStream(), responsesFailedStreamFrames...)
	t.Logf("Anthropic wire:\n%s", wire)
	if strings.Contains(string(wire), `"stop_reason":"end_turn"`) {
		t.Fatalf("response.failed must not produce Anthropic stop_reason=end_turn; got:\n%s", wire)
	}
}

// Responses response.failed buffered → cross-shape to Anthropic must not emit end_turn.
func TestCrossShape_ResponsesFailed_AnthropicBuffered_NotEndTurn(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"The model produced invalid output"}}`)
	resp, err := openai.ResponsesTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusFailed {
		t.Fatalf("precondition: expected failed, got %q", resp.Status)
	}
	out, err := anthropic.AnthropicTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Anthropic wire: %s", out)
	if strings.Contains(string(out), `"stop_reason":"end_turn"`) {
		t.Fatalf("status=failed with empty finish_reason must not produce Anthropic stop_reason=end_turn; got:\n%s", out)
	}
}

// --- Gemini stream tests ---

// Responses response.failed stream → cross-shape to Gemini stream must not emit finishReason=STOP.
func TestCrossShape_ResponsesFailed_GeminiStream_NotStop(t *testing.T) {
	_, wire := xsPipe(t, openai.ResponsesTranslator{}.NewToCanonicalStream(), gemini.GeminiTranslator{}.NewFromCanonicalStream(), responsesFailedStreamFrames...)
	t.Logf("Gemini wire:\n%s", wire)
	if strings.Contains(string(wire), `"finishReason":"STOP"`) {
		t.Fatalf("response.failed must not produce Gemini finishReason=STOP; got:\n%s", wire)
	}
}

// Anthropic unknown stop_reason stream → cross-shape to Gemini stream must not emit finishReason=STOP.
func TestCrossShape_AnthropicUnknownStop_GeminiStream_NotStop(t *testing.T) {
	_, wire := xsPipe(t, anthropic.AnthropicTranslator{}.NewToCanonicalStream(), gemini.GeminiTranslator{}.NewFromCanonicalStream(), anthropicUnknownStopFrames...)
	t.Logf("Gemini wire:\n%s", wire)
	if strings.Contains(string(wire), `"finishReason":"STOP"`) {
		t.Fatalf("Anthropic unknown stop_reason (status=incomplete) must not produce Gemini finishReason=STOP; got:\n%s", wire)
	}
}

// Responses response.failed buffered → cross-shape to Gemini must not emit finishReason=STOP.
func TestCrossShape_ResponsesFailed_GeminiBuffered_NotStop(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"The model produced invalid output"}}`)
	resp, err := openai.ResponsesTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gemini.GeminiTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Gemini wire: %s", out)
	if strings.Contains(string(out), `"finishReason":"STOP"`) {
		t.Fatalf("status=failed with empty finish_reason must not produce Gemini finishReason=STOP; got:\n%s", out)
	}
}

// Anthropic unknown stop_reason buffered → cross-shape to Gemini must not emit finishReason=STOP.
func TestCrossShape_AnthropicUnknownStop_GeminiBuffered_NotStop(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"partial"}],"stop_reason":"model_context_window_exceeded","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`)
	resp, err := anthropic.AnthropicTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gemini.GeminiTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Gemini wire: %s", out)
	if strings.Contains(string(out), `"finishReason":"STOP"`) {
		t.Fatalf("Anthropic unknown stop_reason (status=incomplete, finish=other) must not produce Gemini finishReason=STOP; got:\n%s", out)
	}
}

// --- unknown finish reason matrix ---

// unknownFinishWire is one wire shape's unknown-reason fixtures and the field
// its terminal reason is read from.
type unknownFinishWire struct {
	name   string
	tr     v1.Translator
	body   []byte
	stream [][]byte
	// raw is the canonical incomplete_details.reason the unknown reason parses to.
	raw string
	// read pulls the terminal reason out of one body or frame of this wire.
	read func(m map[string]any) string
}

func unknownFinishWires() []unknownFinishWire {
	str := func(v any) string { s, _ := v.(string); return s }
	first := func(v any) map[string]any {
		if a, ok := v.([]any); ok && len(a) > 0 {
			m, _ := a[0].(map[string]any)
			return m
		}
		return nil
	}
	return []unknownFinishWire{
		{
			name: "cc",
			tr:   openai.CCTranslator{},
			body: []byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"new_reason"}]}`),
			stream: [][]byte{
				xsSSE("", `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"x"},"finish_reason":null}]}`),
				xsSSE("", `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"new_reason"}]}`),
				xsSSE("", `[DONE]`),
			},
			raw:  "openai:new_reason",
			read: func(m map[string]any) string { return str(first(m["choices"])["finish_reason"]) },
		},
		{
			name: "responses",
			tr:   openai.ResponsesTranslator{},
			body: []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"new_reason"},"model":"m","output":[]}`),
			stream: [][]byte{
				xsSSE("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`),
				xsSSE("response.incomplete", `{"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"new_reason"},"model":"m","output":[]}}`),
			},
			raw: "new_reason",
			read: func(m map[string]any) string {
				if r, ok := m["response"].(map[string]any); ok {
					m = r
				}
				status := str(m["status"])
				if status == "" || status == "in_progress" {
					return ""
				}
				reason := ""
				if d, ok := m["incomplete_details"].(map[string]any); ok {
					reason = str(d["reason"])
				}
				return status + "/" + reason
			},
		},
		{
			name: "anthropic",
			tr:   anthropic.AnthropicTranslator{},
			body: []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"new_reason","usage":{"input_tokens":1,"output_tokens":1}}`),
			stream: [][]byte{
				xsSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
				xsSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				xsSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`),
				xsSSE("content_block_stop", `{"type":"content_block_stop","index":0}`),
				xsSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"new_reason","stop_sequence":null},"usage":{"output_tokens":1}}`),
				xsSSE("message_stop", `{"type":"message_stop"}`),
			},
			raw: "anthropic:new_reason",
			read: func(m map[string]any) string {
				if d, ok := m["delta"].(map[string]any); ok {
					return str(d["stop_reason"])
				}
				return str(m["stop_reason"])
			},
		},
		{
			name: "gemini",
			tr:   gemini.GeminiTranslator{},
			body: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"NEW_REASON","index":0}],"modelVersion":"m"}`),
			stream: [][]byte{
				xsSSE("", `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"index":0}],"modelVersion":"m"}`),
				xsSSE("", `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"NEW_REASON","index":0}],"modelVersion":"m"}`),
			},
			raw:  "gemini:NEW_REASON",
			read: func(m map[string]any) string { return str(first(m["candidates"])["finishReason"]) },
		},
	}
}

// unknownFinishWant is the terminal reason each target wire (inner key) gets
// for each source wire's unknown reason. The diagonal is the same-vendor
// round trip.
var unknownFinishWant = map[string]map[string]string{
	"cc":        {"cc": "new_reason", "responses": "incomplete/openai:new_reason", "anthropic": "refusal", "gemini": "OTHER"},
	"responses": {"cc": "content_filter", "responses": "incomplete/new_reason", "anthropic": "refusal", "gemini": "OTHER"},
	"anthropic": {"cc": "content_filter", "responses": "incomplete/anthropic:new_reason", "anthropic": "new_reason", "gemini": "OTHER"},
	"gemini":    {"cc": "content_filter", "responses": "incomplete/gemini:NEW_REASON", "anthropic": "refusal", "gemini": "NEW_REASON"},
}

// lastWireReason returns the last non-empty terminal reason across frames.
func lastWireReason(w unknownFinishWire, frames [][]byte) string {
	got := ""
	for _, f := range frames {
		data := f
		if _, d, ok := v1.ParseSSEChunk(f); ok {
			data = d
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if r := w.read(m); r != "" {
			got = r
		}
	}
	return got
}

func TestUnknownFinishReason_Buffered(t *testing.T) {
	wires := unknownFinishWires()
	for _, src := range wires {
		resp, err := src.tr.ParseResponse(src.body)
		if err != nil {
			t.Fatalf("%s parse: %v", src.name, err)
		}
		if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther || resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != src.raw {
			t.Errorf("%s parse = %q/%q/%+v, want incomplete/other/%s", src.name, resp.Status, resp.FinishReason, resp.IncompleteDetails, src.raw)
		}
		for _, dst := range wires {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				out, err := dst.tr.SerializeResponse(resp, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := lastWireReason(dst, [][]byte{out}), unknownFinishWant[src.name][dst.name]; got != want {
					t.Errorf("reason = %q, want %q\n%s", got, want, out)
				}
			})
		}
	}
}

func TestUnknownFinishReason_Stream(t *testing.T) {
	wires := unknownFinishWires()
	for _, src := range wires {
		for _, dst := range wires {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				canon, wire := xsPipe(t, src.tr.NewToCanonicalStream(), dst.tr.NewFromCanonicalStream(), src.stream...)
				var done v1.GenerationCompletedEvent
				for _, f := range xsSplit(canon) {
					if ev, data, _ := v1.ParseSSEChunk(f); ev == v1.EventGenerationCompleted {
						if err := json.Unmarshal(data, &done); err != nil {
							t.Fatal(err)
						}
					}
				}
				if done.Status != v1.StatusIncomplete || done.FinishReason != v1.FinishReasonOther || done.IncompleteDetails == nil || done.IncompleteDetails.Reason != src.raw {
					t.Errorf("canonical completed = %q/%q/%+v, want incomplete/other/%s", done.Status, done.FinishReason, done.IncompleteDetails, src.raw)
				}
				if got, want := lastWireReason(dst, xsSplit(wire)), unknownFinishWant[src.name][dst.name]; got != want {
					t.Errorf("reason = %q, want %q\n%s", got, want, wire)
				}
			})
		}
	}
}

// A canonical caller may send other without a raw reason; no wire may read it
// as success.
func TestOtherFinishReason_WithoutDetails(t *testing.T) {
	want := map[string]string{"cc": "content_filter", "responses": "incomplete/", "anthropic": "refusal", "gemini": "OTHER"}
	resp := &v1.Response{ID: "r1", Model: "m", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther}
	created, _ := json.Marshal(v1.GenerationCreatedEvent{ID: "r1", Model: "m"})
	completed, _ := json.Marshal(v1.GenerationCompletedEvent{ID: "r1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther})
	for _, w := range unknownFinishWires() {
		t.Run(w.name, func(t *testing.T) {
			out, err := w.tr.SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := lastWireReason(w, [][]byte{out}); got != want[w.name] {
				t.Errorf("buffered reason = %q, want %q\n%s", got, want[w.name], out)
			}
			fromCanon := w.tr.NewFromCanonicalStream()
			var wire []byte
			for _, f := range [][]byte{xsSSE(v1.EventGenerationCreated, string(created)), xsSSE(v1.EventGenerationCompleted, string(completed))} {
				b, err := fromCanon(f)
				if err != nil {
					t.Fatal(err)
				}
				wire = append(wire, b...)
			}
			if got := lastWireReason(w, xsSplit(wire)); got != want[w.name] {
				t.Errorf("stream reason = %q, want %q\n%s", got, want[w.name], wire)
			}
		})
	}
}
