package openai_test

// Cross-shape terminal-status tests. Each serializer must NOT emit a success
// signal when the canonical response carries a non-success status with an
// empty finish_reason (rule 11: no silent drops).

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
	if resp.Status != v1.StatusIncomplete || resp.FinishReason != "" {
		t.Fatalf("precondition: expected incomplete/empty, got %q/%q", resp.Status, resp.FinishReason)
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
		t.Fatalf("Anthropic unknown stop_reason (status=incomplete, finish=\"\") must not produce CC finish_reason=stop; got %q", r.Choices[0].FinishReason)
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
		t.Fatalf("Anthropic unknown stop_reason (status=incomplete, finish=\"\") must not produce Gemini finishReason=STOP; got:\n%s", out)
	}
}
