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

// TestResponsesReasoningRoundTrip guards the stateless reasoning/tool pairing:
// the upstream request must ask for the encrypted reasoning blob (include +
// store:false), and a reasoning item's encrypted_content must survive
// Responses->canonical->Responses so the function_call keeps its required
// reasoning sibling on the next tool-loop turn.
func TestResponsesReasoningRoundTrip(t *testing.T) {
	// 1. Request asks OpenAI for the blob and doesn't persist server-side.
	reqBody, err := ResponsesTranslator{}.SerializeRequest(&v1.Request{
		Model: v1.ModelRefs{"gpt-5-5"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reqBody), `"reasoning.encrypted_content"`) {
		t.Errorf("BUG: request missing include reasoning.encrypted_content: %s", reqBody)
	}
	if !strings.Contains(string(reqBody), `"store":false`) {
		t.Errorf("BUG: request missing store:false: %s", reqBody)
	}

	// 2. A reasoning item with encrypted_content round-trips intact.
	respBody := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"reasoning","id":"rs_abc","encrypted_content":"BLOB123","summary":[]},{"type":"function_call","id":"fc_xyz","call_id":"call_1","name":"f","arguments":"{}"}]}`
	canon, err := ResponsesTranslator{}.ParseResponse([]byte(respBody))
	if err != nil {
		t.Fatal(err)
	}
	// Feed the canonical items back as a follow-up request's input.
	back, err := ResponsesTranslator{}.SerializeRequest(&v1.Request{
		Model: v1.ModelRefs{"gpt-5-5"},
		Input: canon.Output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "BLOB123") {
		t.Fatalf("BUG: encrypted_content lost on round-trip; reasoning sibling would 400: %s", back)
	}
	if !strings.Contains(string(back), `"rs_abc"`) || !strings.Contains(string(back), `"fc_xyz"`) {
		t.Fatalf("BUG: reasoning/function_call ids not both present: %s", back)
	}
}

// TestResponsesReasoning_SummaryAlwaysEmitted guards a required-field omission:
// a reasoning item commonly has no summary, but the Responses API rejects an
// input reasoning item without one ("Missing required parameter
// input[N].summary"). It must serialize as [] — never null, never omitted.
func TestResponsesReasoning_SummaryAlwaysEmitted(t *testing.T) {
	b, err := (&ResponsesReasoning{ID: "rs_1", EncryptedContent: "blob"}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"summary":[]`) {
		t.Fatalf("BUG: reasoning item missing summary:[] : %s", b)
	}

	// Round-trip: a no-summary reasoning item fed back as input keeps summary.
	resp := `{"id":"r","object":"response","status":"completed","model":"m","output":[{"type":"reasoning","id":"rs_2","encrypted_content":"X","summary":[]},{"type":"function_call","id":"fc","call_id":"c","name":"f","arguments":"{}"}]}`
	canon, err := ResponsesTranslator{}.ParseResponse([]byte(resp))
	if err != nil {
		t.Fatal(err)
	}
	back, err := ResponsesTranslator{}.SerializeRequest(&v1.Request{Model: v1.ModelRefs{"m"}, Input: canon.Output})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), `"summary":[]`) {
		t.Fatalf("BUG: round-tripped reasoning item dropped summary: %s", back)
	}
}
