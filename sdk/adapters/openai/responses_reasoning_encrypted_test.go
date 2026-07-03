package openai

import (
	"strings"
	"testing"
)

// TestResponsesStream_ReasoningEncryptedContent guards the stateless reasoning
// round-trip on the STREAMING path: a reasoning item's encrypted_content
// (returned because SerializeRequest asks for include:
// ["reasoning.encrypted_content"] with store:false) must reach the canonical
// stream as a Reasoning item.completed carrying provider_data — otherwise the
// caller replays function_call items without their required reasoning siblings.
// The sync-path equivalent lives in responses_canonical_log_test.go.
func TestResponsesStream_ReasoningEncryptedContent(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()
	chunks := []string{
		`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5","status":"in_progress"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"BLOB123"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"f","arguments":""}}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
	}

	var all strings.Builder
	for i, c := range chunks {
		out, err := toCanon([]byte(c + "\n\n"))
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		all.Write(out)
	}
	out := all.String()

	if !strings.Contains(out, `"type":"reasoning"`) {
		t.Errorf("BUG: no reasoning item.completed on canonical stream:\n%s", out)
	}
	if !strings.Contains(out, "BLOB123") {
		t.Errorf("BUG: encrypted_content dropped from canonical stream (replay loses reasoning sibling):\n%s", out)
	}
	if !strings.Contains(out, `"item_id":"rs_1"`) {
		t.Errorf("BUG: reasoning item id missing from canonical stream:\n%s", out)
	}
}
