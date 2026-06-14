package openai

import (
	"strings"
	"testing"
)

// TestResponsesStream_ReasoningSummaryDeltas guards the reasoning-summary
// channel: a summary-mode model (gpt-5.5) encrypts its raw reasoning_text and
// streams the human-readable thinking ONLY over response.reasoning_summary_text
// events. The translator must map those deltas to canonical reasoning deltas
// (so the thinking renders live) and backfill the terminal reasoning item's
// summary (which gpt-5.5 sends empty).
func TestResponsesStream_ReasoningSummaryDeltas(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()
	chunks := []string{
		`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5","status":"in_progress"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Let me "}`,
		`event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think."}`,
		`event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"Let me think."}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"BLOB"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.5","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"BLOB"}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
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

	// Live glow: the summary deltas became canonical reasoning deltas.
	if !strings.Contains(out, `"kind":"reasoning"`) {
		t.Fatalf("BUG: no canonical reasoning delta frames emitted: %s", out)
	}
	if !strings.Contains(out, "Let me ") || !strings.Contains(out, "think.") {
		t.Fatalf("BUG: reasoning summary delta text dropped: %s", out)
	}

	// Accumulation: the terminal reasoning item carries the full summary even
	// though its output_item.done arrived empty. The contiguous string only
	// appears in the backfilled summary (deltas are split across two frames).
	if !strings.Contains(out, `"text":"Let me think."`) {
		t.Fatalf("BUG: terminal reasoning item missing accumulated summary: %s", out)
	}
}
