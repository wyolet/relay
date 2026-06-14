package openai

import (
	"strings"
	"testing"
)

// TestResponsesStreamCrossShape_TerminalChunkCarriesUsage pipes a realistic
// OpenAI Responses streaming sequence through the cross-shape chain the relay
// uses for a CC-inbound request routed to a Responses upstream
// (NewToCanonicalStream -> NewFromCanonicalStream), and checks that text deltas
// flow AND the terminal CC chunk carries finish_reason + usage. Regression for
// the dropped response.completed event (polymorphic-output unmarshal swallowed
// the terminal frame, so streamed usage never reached the caller).
func TestResponsesStreamCrossShape_TerminalChunkCarriesUsage(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()
	fromCanon := CCTranslator{}.NewFromCanonicalStream()

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
		`event: response.output_text.done
data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello world"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}}`,
	}

	var ccOut strings.Builder
	for i, c := range chunks {
		canon, err := toCanon([]byte(c + "\n\n"))
		if err != nil {
			t.Fatalf("toCanon chunk %d: %v", i, err)
		}
		if len(canon) == 0 {
			continue
		}
		// Mirror dispatch.streamCanonical: split canonical SSE frames on the
		// blank-line boundary, run each through fromCanon.
		for _, f := range strings.Split(strings.TrimRight(string(canon), "\n"), "\n\n") {
			if strings.TrimSpace(f) == "" {
				continue
			}
			cc, err := fromCanon([]byte(f + "\n\n"))
			if err != nil {
				t.Fatalf("fromCanon chunk %d: %v", i, err)
			}
			ccOut.Write(cc)
		}
	}

	out := ccOut.String()
	t.Logf("=== CC OUTPUT relay would emit ===\n%s\n=== end ===", out)

	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Errorf("BUG: text deltas did not flow through to CC output")
	}
	if !strings.Contains(out, "prompt_tokens") || !strings.Contains(out, "5504") {
		t.Errorf("BUG: usage missing from CC terminal chunk (want prompt_tokens=5504)")
	}
}

// TestResponsesToCanonicalStream_TerminalUsage isolates the responses->canonical
// step (what a /v1/generate identity-inbound client receives): the streamed
// response.completed event must yield a canonical generation.completed frame
// carrying the orthogonal-meter usage. This is the exact step the SDK tripped on
// (polymorphic []ResponsesItem output broke the terminal unmarshal).
func TestResponsesToCanonicalStream_TerminalUsage(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()

	completed := `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}}`

	canon, err := toCanon([]byte(completed + "\n\n"))
	if err != nil {
		t.Fatalf("toCanon: %v", err)
	}
	got := string(canon)
	t.Logf("=== canonical output for response.completed ===\n%s", got)

	if !strings.Contains(got, "generation.completed") {
		t.Fatalf("BUG: response.completed produced no canonical generation.completed frame (got: %q)", got)
	}
	// Orthogonal meters: input de-overlapped from cache (5504-5120=384),
	// output, cache_read, reasoning each present.
	for _, want := range []string{`"input":384`, `"output":104`, `"cache_read":5120`, `"reasoning":49`} {
		if !strings.Contains(got, want) {
			t.Errorf("BUG: canonical usage missing %s\n full: %s", want, got)
		}
	}
}

// TestResponsesToCanonical_BufferedUsage is the non-streaming counterpart:
// ParseResponse must carry usage off the full response body.
func TestResponsesToCanonical_BufferedUsage(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}`
	resp, err := ResponsesTranslator{}.ParseResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.Usage["input"] != 384 || resp.Usage["output"] != 104 ||
		resp.Usage["cache_read"] != 5120 || resp.Usage["reasoning"] != 49 {
		t.Fatalf("BUG: buffered canonical usage wrong: %+v", resp.Usage)
	}
}
