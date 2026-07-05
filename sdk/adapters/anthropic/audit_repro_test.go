// Bug-reproduction tests for the 2026-07-04 audit (audit-sdk-adapters.md).
// Each test asserts the CORRECT wire behavior and is expected to FAIL until
// the corresponding finding is fixed; once red is confirmed they are t.Skip'd
// with an audit marker so the suite stays green.
package anthropic

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Audit P1 tracker #12 (#370 follow-up): replaying a prior assistant turn that
// contained thinking + visible text + a tool call must serialize to ONE
// Anthropic assistant message whose content leads with the signed thinking
// block and carries the tool_use in the same message.
//
// The item sequence is reconstructed realistically: ParseResponse of an
// Anthropic thinking+text+tool_use response yields
// [Reasoning, Message(assistant), FunctionCall] — exactly what the relay's
// own sync/streaming path hands back to a canonical client, and what the
// client replays verbatim on the next turn of a tool loop.
//
// Today the assistant Message consumes pendingThinking (msg1 = [thinking,
// text]) and the trailing FunctionCall is flushed into a SECOND assistant
// message (msg2 = [tool_use] with no thinking). Anthropic rejects that with
// "messages.N.content.0.type: Expected thinking or redacted_thinking, but
// found tool_use" whenever thinking is enabled, so every canonical-inbound
// tool loop 400s on turn two when turn one had visible text beside the call.
func TestAnthropicSerializeRequest_ThinkingTextToolUseReplay_SingleAssistantMessage(t *testing.T) {
	const model = "claude-fable-5"

	// Turn 1 upstream response: thinking + text + tool_use, as Anthropic sends it.
	respBody := mustJSON(map[string]any{
		"id":          "msg_prev",
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"stop_reason": "tool_use",
		"content": []map[string]any{
			{"type": "thinking", "thinking": "Need the live weather first.", "signature": "SIGTOOL1"},
			{"type": "text", "text": "Let me check the weather."},
			{"type": "tool_use", "id": "toolu_w1", "name": "get_weather", "input": map[string]any{"city": "Tashkent"}},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 25},
	})
	prev, err := (AnthropicTranslator{}).ParseResponse(respBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev.Output) != 3 {
		t.Fatalf("precondition: ParseResponse items = %d, want 3 [Reasoning, Message, FunctionCall]: %#v", len(prev.Output), prev.Output)
	}

	// Turn 2 request: user turn + verbatim replay of turn 1 + the tool result.
	input := []v1.Item{
		&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "weather in Tashkent?"}}},
	}
	input = append(input, prev.Output...)
	input = append(input, &v1.FunctionCallOutput{CallID: "toolu_w1", Output: "sunny, 34C"})

	req := &v1.Request{
		Model:      v1.ModelRefs{model},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			model: {Reasoning: &v1.ReasoningConfig{}}, // thinking enabled (adaptive)
		},
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		},
		Input: input,
	}

	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs, _ := m["messages"].([]any)

	var assistants [][]any // content blocks per assistant message
	for _, raw := range msgs {
		msg := raw.(map[string]any)
		if msg["role"] != "assistant" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		if blocks == nil {
			if s, ok := msg["content"].(string); ok {
				blocks = []any{map[string]any{"type": "text", "text": s}}
			}
		}
		assistants = append(assistants, blocks)
	}

	if len(assistants) != 1 {
		t.Fatalf("replayed thinking+text+tool_use turn must serialize to ONE assistant message, got %d\nwire body: %s", len(assistants), out)
	}

	blocks := assistants[0]
	hasToolUse := false
	for _, b := range blocks {
		if b.(map[string]any)["type"] == "tool_use" {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Fatalf("assistant message lost the tool_use block: %v\nwire body: %s", blocks, out)
	}
	if first := blocks[0].(map[string]any); first["type"] != "thinking" {
		// Anthropic: "messages.N.content.0.type: Expected thinking or
		// redacted_thinking, but found tool_use" — the tool_use-bearing
		// assistant turn must begin with its signed thinking block.
		t.Fatalf("tool_use-bearing assistant message must lead with the signed thinking block; content[0] = %v\nwire body: %s", first, out)
	}
}

// Audit P2 (DECISION #3): a canonical request with thinking enabled AND a
// structured-output format must not serialize to a body that combines an
// enabled/adaptive thinking block with a forced tool_choice — Anthropic
// rejects forced tool choice (type "tool"/"any") whenever thinking is on
// (only auto/none are allowed), so the relay would manufacture a guaranteed
// upstream 400 out of a valid canonical request.
//
// The safe behavior (per the audit's suggested fix): keep the synthetic
// structured-output tool but do NOT force tool_choice when thinking is
// enabled. This test asserts only the non-contradiction — whatever
// resolution lands, the wire body must not carry both halves of the 400.
func TestAnthropicSerializeRequest_StructuredOutputWithThinking_NoForcedToolChoice(t *testing.T) {
	t.Skip("audit 2026-07-04: structured-output forces tool_choice while thinking enabled — known-broken, unskip with the fix")
	const model = "claude-fable-5"
	req := &v1.Request{
		Model:      v1.ModelRefs{model},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			model: {
				Reasoning: &v1.ReasoningConfig{}, // adaptive thinking
				Output: &v1.OutputConfig{Format: &v1.Format{
					Type:   "json_schema",
					Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
				}},
			},
		},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "answer"}}},
		},
	}

	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)

	thinking, thinkingOn := m["thinking"].(map[string]any)
	if !thinkingOn || (thinking["type"] != "enabled" && thinking["type"] != "adaptive") {
		t.Fatalf("precondition: thinking should be enabled/adaptive on the wire, got %v", m["thinking"])
	}

	if tc, ok := m["tool_choice"].(map[string]any); ok {
		if tc["type"] == "tool" || tc["type"] == "any" {
			t.Fatalf("thinking enabled + forced tool_choice is an Anthropic-guaranteed 400 (only auto/none allowed with thinking); got tool_choice %v\nwire body: %s", tc, out)
		}
	}
}
