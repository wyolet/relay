package openai

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// realistic full buffered Responses response (message output_text + usage).
const respBufferedBody = `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}`

// TestLog_ResponsesToCanonical_Buffered logs exactly what a canonical SDK client
// (/v1/generate, identity inbound) receives for a buffered Responses upstream
// response: ParseResponse (responses->canonical) then identity SerializeResponse.
func TestLog_ResponsesToCanonical_Buffered(t *testing.T) {
	resp, err := ResponsesTranslator{}.ParseResponse([]byte(respBufferedBody))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	out, err := v1.IdentityTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatalf("identity SerializeResponse: %v", err)
	}
	t.Logf("=== BUFFERED canonical body relay sends to SDK client ===\n%s\n=== end ===", out)

	if !strings.Contains(string(out), "Hello world") {
		t.Errorf("EMPTY OUTPUT: assistant text missing from canonical body")
	}
	if !strings.Contains(string(out), `"output":104`) {
		t.Errorf("usage missing from canonical body")
	}
}

// TestLog_ResponsesToCanonical_Streaming logs every canonical SSE frame a
// canonical SDK client receives (identity fromCanon is a no-op, so toCanon's
// output IS the wire). Covers created/delta/completed.
func TestLog_ResponsesToCanonical_Streaming(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()
	chunks := []string{
		`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"in_progress"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
		`event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}}`,
	}
	var all strings.Builder
	for i, c := range chunks {
		canon, err := toCanon([]byte(c + "\n\n"))
		if err != nil {
			t.Fatalf("toCanon chunk %d: %v", i, err)
		}
		all.Write(canon)
	}
	out := all.String()
	t.Logf("=== STREAMING canonical frames relay sends to SDK client ===\n%s\n=== end ===", out)

	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Errorf("EMPTY OUTPUT: text deltas did not produce canonical frames")
	}
	if !strings.Contains(out, "generation.completed") {
		t.Errorf("no terminal generation.completed frame")
	}
}

// TestResponsesSerializeRequest_StreamFlagPropagates guards the streaming bug:
// SerializeRequest builds its own wireReq literal, which once omitted Stream
// (and StopSequences) — so a stream request went upstream as a buffered one,
// the JSON came back, and streamCanonical scanned it as SSE -> empty output.
func TestResponsesSerializeRequest_StreamFlagPropagates(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-5-5"},
		Input:      []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		OutputMode: v1.OutputModeStream,
	}
	body, err := ResponsesTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("BUG: stream:true not set on upstream request: %s", body)
	}
}
