package anthropic

import (
	"bytes"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const (
	wireMessageStart = `{"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":1}}}`
	wireBlockStop    = `{"type":"content_block_stop","index":0}`
	wireMessageStop  = `{"type":"message_stop"}`
	canonCreated     = `{"id":"msg_1","model":"claude-test"}`
)

func TestToCanonicalStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire [][]byte
		want []string
	}{
		{
			name: "text",
			wire: frames(
				"message_start", wireMessageStart,
				"ping", `{"type":"ping"}`,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
				"content_block_stop", wireBlockStop,
				"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				"message_stop", wireMessageStop,
			),
			want: []string{
				v1.EventGenerationCreated, canonCreated,
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"message","index":0}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"text","delta":"Hel"}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"text","delta":"lo"}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"message","id":"t_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"completed","finish_reason":"stop","usage":{"input":10,"output":5}}`,
			},
		},
		{
			name: "signed thinking then tool call",
			wire: frames(
				"message_start", wireMessageStart,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pl"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"an"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG"}}`,
				"content_block_stop", wireBlockStop,
				"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"search","input":{}}}`,
				"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":1}`,
				"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
				"message_stop", wireMessageStop,
			),
			want: []string{
				v1.EventGenerationCreated, canonCreated,
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"reasoning","index":0}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"reasoning","delta":"pl"}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"reasoning","delta":"an"}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"reasoning","id":"t_0","summary":[{"text":"plan"}],"content":"plan","status":"completed","provider_data":{"signature":"SIG","thinking":"plan","type":"thinking"}}}`,
				v1.EventItemStarted, `{"item_id":"t_1","item_type":"function_call","index":1,"name":"search","call_id":"toolu_1"}`,
				v1.EventItemDelta, `{"item_id":"t_1","index":1,"kind":"arguments","delta":"{\"q\":"}`,
				v1.EventItemDelta, `{"item_id":"t_1","index":1,"kind":"arguments","delta":"\"x\"}"}`,
				v1.EventItemCompleted, `{"item_id":"t_1","index":1,"item":{"type":"function_call","id":"t_1","call_id":"toolu_1","name":"search","arguments":"{\"q\":\"x\"}","status":"completed"}}`,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"completed","finish_reason":"tool_calls","usage":{"input":10,"output":7}}`,
			},
		},
		{
			// Eager input streaming lets a cut-off turn close the block on unterminated JSON.
			name: "tool call cut off mid-arguments is incomplete",
			wire: frames(
				"message_start", wireMessageStart,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"unter"}}`,
				"content_block_stop", wireBlockStop,
				"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":7}}`,
				"message_stop", wireMessageStop,
			),
			want: []string{
				v1.EventGenerationCreated, canonCreated,
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"function_call","index":0,"name":"search","call_id":"toolu_1"}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"arguments","delta":"{\"q\":\"unter"}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"function_call","id":"t_0","call_id":"toolu_1","name":"search","arguments":"{\"q\":\"unter","status":"incomplete"}}`,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"incomplete","finish_reason":"length","usage":{"input":10,"output":7},"incomplete_details":{"reason":"max_output_tokens"}}`,
			},
		},
		{
			name: "tool call with no argument deltas is completed",
			wire: frames(
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"now"}}`,
				"content_block_stop", wireBlockStop,
			),
			want: []string{
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"function_call","index":0,"name":"now","call_id":"toolu_1"}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"function_call","id":"t_0","call_id":"toolu_1","name":"now","arguments":"","status":"completed"}}`,
			},
		},
		{
			name: "blocks and deltas with no canonical form emit nothing",
			wire: frames(
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				"content_block_stop", wireBlockStop,
				"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"x"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":1}`,
				"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"text":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"lost"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":2}`,
				"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"citations_delta","citation":{}}}`,
				"content_block_stop", `{"type":"content_block_stop","index":3}`,
				"future_event", `{}`,
			),
			want: []string{
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"message","index":0}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"message","id":"t_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":""}]}}`,
			},
		},
		{
			name: "usage takes the larger of message_start and message_delta, tier from either",
			wire: frames(
				"message_start", `{"message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":1000,"cache_read_input_tokens":30,"cache_creation_input_tokens":4000,"cache_creation":{"ephemeral_1h_input_tokens":1500}}}}`,
				"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":300,"input_tokens":1001,"cache_read_input_tokens":0,"service_tier":"priority"}}`,
				"message_stop", wireMessageStop,
			),
			want: []string{
				v1.EventGenerationCreated, canonCreated,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"completed","finish_reason":"stop","usage":{"input":1001,"output":300,"cache_read":30,"cache_creation":4000,"cache_creation_1h":1500},"service_tier":"priority"}`,
			},
		},
		{
			name: "error event",
			wire: frames("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			want: []string{v1.EventError, `{"code":"overloaded_error","message":"Overloaded"}`},
		},
		{
			name: "error event without a message carries the raw payload",
			wire: frames("error", `{"detail":"x"}`),
			want: []string{v1.EventError, `{"code":"","message":"{\"detail\":\"x\"}"}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateAll(t, AnthropicTranslator{}.NewToCanonicalStream(), tc.wire)
			assertEvents(t, got, tc.want...)
			joined := translateAll(t, AnthropicTranslator{}.NewToCanonicalStream(), [][]byte{bytes.Join(tc.wire, nil)})
			if !bytes.Equal(joined, got) {
				t.Errorf("all frames in one chunk:\ngot  %s\nwant %s", joined, got)
			}
		})
	}
}

func TestToCanonicalStream_MessageStartWithoutIDGetsOne(t *testing.T) {
	out := translateAll(t, AnthropicTranslator{}.NewToCanonicalStream(), frames("message_start", `{"message":{"model":"m"}}`, "message_stop", `{}`))
	events := decodeSSE(t, out)
	created, completed := events[0].Data.(map[string]any)["id"], events[1].Data.(map[string]any)["id"]
	if created == "" || created != completed {
		t.Errorf("ids: created %q, completed %q, want one non-empty id", created, completed)
	}
}

func TestFromCanonicalStream(t *testing.T) {
	const wireStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
	for _, tc := range []struct {
		name  string
		canon [][]byte
		want  []string
	}{
		{
			// The canonical index gap (0, 2, 3) must not reach the wire: Anthropic block indexes are dense.
			name: "text, tool call and signed reasoning",
			canon: frames(
				v1.EventGenerationCreated, canonCreated,
				v1.EventItemStarted, `{"item_id":"msg_0","item_type":"message","index":0}`,
				v1.EventItemDelta, `{"item_id":"msg_0","index":0,"kind":"text","delta":"Hi"}`,
				v1.EventItemCompleted, `{"item_id":"msg_0","index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}}`,
				v1.EventItemStarted, `{"item_id":"fc_2","item_type":"function_call","index":2,"name":"search","call_id":"call_2"}`,
				v1.EventItemDelta, `{"item_id":"fc_2","index":2,"kind":"arguments","delta":"{\"q\":1}"}`,
				v1.EventItemCompleted, `{"item_id":"fc_2","index":2,"item":{"type":"function_call","call_id":"call_2","name":"search","arguments":"{\"q\":1}"}}`,
				v1.EventItemStarted, `{"item_id":"rs_3","item_type":"reasoning","index":3}`,
				v1.EventItemDelta, `{"item_id":"rs_3","index":3,"kind":"reasoning","delta":"plan"}`,
				v1.EventItemCompleted, `{"item_id":"rs_3","index":3,"item":{"type":"reasoning","content":"plan","provider_data":{"type":"thinking","thinking":"plan","signature":"SIG"}}}`,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"completed","finish_reason":"tool_calls","usage":{"input":1000,"output":300,"cache_read":30,"cache_creation":4000,"cache_creation_1h":1500},"service_tier":"priority"}`,
			),
			want: []string{
				"message_start", wireStart,
				"ping", `{"type":"ping"}`,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":0}`,
				"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_2","name":"search","input":{}}}`,
				"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":1}`,
				"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"SIG"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":2}`,
				"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":""},"usage":{"input_tokens":1000,"output_tokens":300,"cache_read_input_tokens":30,"cache_creation_input_tokens":4000,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500}}}`,
				"message_stop", wireMessageStop,
			},
		},
		{
			name: "events for items that were never opened emit nothing",
			canon: frames(
				v1.EventItemDelta, `{"item_id":"x","index":5,"kind":"text","delta":"lost"}`,
				v1.EventItemCompleted, `{"item_id":"x","index":5,"item":{"type":"message","role":"assistant","content":[]}}`,
				"future.event", `{}`,
			),
		},
		{
			name: "call id falls back to the item id, unsigned reasoning closes without a signature, usage defaults",
			canon: frames(
				v1.EventItemStarted, `{"item_id":"fc_0","item_type":"function_call","index":0,"name":"now"}`,
				v1.EventItemDelta, `{"item_id":"fc_0","index":0,"kind":"future","delta":"x"}`,
				v1.EventItemStarted, `{"item_id":"rs_1","item_type":"reasoning","index":1}`,
				v1.EventItemCompleted, `{"item_id":"rs_1","index":1,"item":{"type":"reasoning","content":"plan","provider_data":{"type":"other","signature":"SIG"}}}`,
				v1.EventGenerationCompleted, `{"id":"msg_1","status":"completed","finish_reason":"stop"}`,
			),
			want: []string{
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"fc_0","name":"now","input":{}}}`,
				"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				"content_block_stop", `{"type":"content_block_stop","index":1}`,
				"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":""},"usage":{"output_tokens":0}}`,
				"message_stop", wireMessageStop,
			},
		},
		{
			name:  "error event",
			canon: frames(v1.EventError, `{"code":"overloaded_error","message":"Overloaded"}`),
			want:  []string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEvents(t, translateAll(t, AnthropicTranslator{}.NewFromCanonicalStream(), tc.canon), tc.want...)
		})
	}
}

// Usage survives canonical → Messages stream → canonical.
func TestFromCanonicalStream_UsageReadsBack(t *testing.T) {
	want := usage.Tokens{"input": 120, "output": 45, "cache_read": 80, "cache_creation": 4000, "cache_creation_1h": 1500}
	completed := mustJSON(v1.GenerationCompletedEvent{ID: "msg_1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop, Usage: want})
	wire := translateAll(t, AnthropicTranslator{}.NewFromCanonicalStream(), frames(v1.EventGenerationCreated, canonCreated, v1.EventGenerationCompleted, string(completed)))
	if diff := cmp.Diff(want, lastGenerationCompleted(t, [][]byte{wire}).Usage); diff != "" {
		t.Errorf("usage read back (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, ExtractTokens(wire)); diff != "" {
		t.Errorf("ExtractTokens on the serialized stream (-want +got):\n%s", diff)
	}
}

// A frame that fails to decode returns an error and no output, and leaves the stream usable.
func TestStreams_MalformedFrameFailsAloneAndStreamContinues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		translate func([]byte) ([]byte, error)
		before    [][]byte
		malformed map[string]string
		after     [][]byte
		want      []string
	}{
		{
			name:      "to canonical",
			translate: AnthropicTranslator{}.NewToCanonicalStream(),
			before: frames(
				"content_block_start", `{"index":0,"content_block":{"type":"text"}}`,
				"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
			),
			malformed: map[string]string{
				"message_start":       `{"message":`,
				"content_block_start": `{"content_block":`,
				"content_block_delta": `{"index":0,"delta":{"type":"text_delta","text":7}}`,
				"message_delta":       `{"delta":`,
			},
			after: frames(
				"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"lo"}}`,
				"content_block_stop", `{"index":0}`,
			),
			want: []string{
				v1.EventItemStarted, `{"item_id":"t_0","item_type":"message","index":0}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"text","delta":"Hel"}`,
				v1.EventItemDelta, `{"item_id":"t_0","index":0,"kind":"text","delta":"lo"}`,
				v1.EventItemCompleted, `{"item_id":"t_0","index":0,"item":{"type":"message","id":"t_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
			},
		},
		{
			name:      "from canonical",
			translate: AnthropicTranslator{}.NewFromCanonicalStream(),
			before: frames(
				v1.EventItemStarted, `{"item_id":"msg_0","item_type":"message","index":0}`,
				v1.EventItemDelta, `{"item_id":"msg_0","index":0,"kind":"text","delta":"Hel"}`,
			),
			malformed: map[string]string{
				v1.EventGenerationCreated:   `{"id":`,
				v1.EventItemStarted:         `{"item_id":`,
				v1.EventItemDelta:           `{"item_id":"msg_0","index":0,"kind":"text","delta":7}`,
				v1.EventItemCompleted:       `{"item_id":`,
				v1.EventGenerationCompleted: `{"id":`,
				v1.EventError:               `{"code":`,
			},
			after: frames(
				v1.EventItemDelta, `{"item_id":"msg_0","index":0,"kind":"text","delta":"lo"}`,
				v1.EventItemCompleted, `{"item_id":"msg_0","index":0}`,
			),
			want: []string{
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
				"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
				"content_block_stop", `{"type":"content_block_stop","index":0}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := translateAll(t, tc.translate, tc.before)
			for event, data := range tc.malformed {
				if b, err := tc.translate(frames(event, data)[0]); err == nil || len(b) != 0 {
					t.Errorf("malformed %s: output %q, err %v, want an error and no output", event, b, err)
				}
			}
			out = append(out, translateAll(t, tc.translate, tc.after)...)
			assertEvents(t, out, tc.want...)
		})
	}
}
