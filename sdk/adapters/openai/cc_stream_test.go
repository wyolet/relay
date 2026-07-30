package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestCCNewToCanonicalStream_RefusalDelta(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunk := ccSSEChunk(map[string]any{
		"id":      "chatcmpl-ref",
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"refusal": "I cannot help.",
			},
		}},
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	events := extractCanonicalEvents(out)
	// Refusal content maps to text delta in canonical.
	var foundDelta bool
	for _, e := range events {
		if e == v1.EventItemDelta {
			foundDelta = true
		}
	}
	if !foundDelta {
		t.Errorf("expected item.delta for refusal, got %v", events)
	}
}

func ccSSEChunk(data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("data: %s\n\n", b))
}

func ccDoneChunk() []byte {
	return []byte("data: [DONE]\n\n")
}

func TestCCNewToCanonicalStream_TextSequence(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-s1",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Hello"},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-s1",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": " world"},
			}},
		}),
		ccDoneChunk(),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	// Must contain generation.created, item.started, item.delta(s), item.completed, generation.completed
	wantContains := []string{
		v1.EventGenerationCreated,
		v1.EventItemStarted,
		v1.EventItemDelta,
		v1.EventItemCompleted,
		v1.EventGenerationCompleted,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestCCNewToCanonicalStream_ToolCallSequence(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-tc",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"index": 0,
						"id":    "call_abc",
						"type":  "function",
						"function": map[string]any{
							"name":      "search",
							"arguments": `{"q":`,
						},
					}},
				},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-tc",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0,
						"function": map[string]any{
							"arguments": `"golang"}`,
						},
					}},
				},
			}},
		}),
		ccDoneChunk(),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	wantContains := []string{v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestCCNewToCanonicalStream_FinishReasonThreaded(t *testing.T) {
	tr := CCTranslator{}

	cases := []struct {
		name       string
		ccReason   string
		wantFinish v1.FinishReason
		wantStatus v1.Status
	}{
		{"tool_calls", "tool_calls", v1.FinishReasonToolCalls, v1.StatusCompleted},
		{"length", "length", v1.FinishReasonLength, v1.StatusIncomplete},
		{"content_filter", "content_filter", v1.FinishReasonContentFilter, v1.StatusCompleted},
		{"stop", "stop", v1.FinishReasonStop, v1.StatusCompleted},
		// No finish_reason chunk at all -> default stop/completed.
		{"absent_defaults_stop", "", v1.FinishReasonStop, v1.StatusCompleted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := tr.NewToCanonicalStream()

			chunks := [][]byte{
				ccSSEChunk(map[string]any{
					"id":      "chatcmpl-fr",
					"object":  "chat.completion.chunk",
					"created": int64(1700000000),
					"model":   "gpt-4o",
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"role": "assistant", "content": "hi"},
					}},
				}),
			}
			if tc.ccReason != "" {
				chunks = append(chunks, ccSSEChunk(map[string]any{
					"id":      "chatcmpl-fr",
					"object":  "chat.completion.chunk",
					"created": int64(1700000000),
					"model":   "gpt-4o",
					"choices": []any{map[string]any{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": tc.ccReason,
					}},
				}))
			}
			chunks = append(chunks, ccDoneChunk())

			var out []byte
			for _, c := range chunks {
				b, err := fn(c)
				if err != nil {
					t.Fatalf("translate: %v", err)
				}
				out = append(out, b...)
			}

			var completed *v1.GenerationCompletedEvent
			for _, frame := range splitCanonicalFrames(out) {
				event, data, ok := v1.ParseSSEChunk(frame)
				if !ok || event != v1.EventGenerationCompleted {
					continue
				}
				var ev v1.GenerationCompletedEvent
				if err := json.Unmarshal(data, &ev); err != nil {
					t.Fatalf("unmarshal completed: %v", err)
				}
				completed = &ev
			}
			if completed == nil {
				t.Fatal("no generation.completed event emitted")
			}
			if completed.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", completed.FinishReason, tc.wantFinish)
			}
			if completed.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", completed.Status, tc.wantStatus)
			}
		})
	}
}

func TestCCNewToCanonicalStream_ReasoningContent(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	// Chunk with non-standard reasoning_content field from o-series upstreams.
	chunk := ccSSEChunk(map[string]any{
		"id":      "chatcmpl-r1",
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   "o1",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"reasoning_content": "Let me think...",
			},
		}},
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	events := extractCanonicalEvents(out)
	var foundDelta bool
	for _, e := range events {
		if e == v1.EventItemDelta {
			foundDelta = true
		}
	}
	if !foundDelta {
		t.Errorf("expected item.delta for reasoning_content, got %v", events)
	}
}

// CC-1: NewFromCanonicalStream must return a non-nil function (was nil → panic).
func TestCCNewFromCanonicalStream_NotNil(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()
	if fn == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}
}

// CC-1: canonical event sequence → valid chat.completion.chunk frames + [DONE].
func TestCCNewFromCanonicalStream_TextSequence(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_1", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello"})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Index: 0, Item: &v1.Message{ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted}})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "resp_1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop, Usage: usage.Tokens{"input": 5, "output": 3}})

	allStr := string(allOut)
	if !strings.Contains(allStr, "chat.completion.chunk") {
		t.Errorf("expected chat.completion.chunk, got: %s", allStr)
	}
	if !strings.Contains(allStr, "Hello") {
		t.Errorf("expected delta text 'Hello'")
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("expected [DONE] terminator")
	}
	if !strings.Contains(allStr, "prompt_tokens") {
		t.Errorf("expected usage in final chunk")
	}
}

// CC-1: tool call streaming emits id+name on first chunk, arguments on delta chunks.
func TestCCNewFromCanonicalStream_ToolCallSequence(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_tc", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_0", ItemType: v1.ItemTypeFunctionCall, Index: 0, Name: "get_weather"})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"loc`})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `ation":"NYC"}`})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "fc_0", Index: 0, Item: &v1.FunctionCall{ID: "fc_0", CallID: "call_abc", Name: "get_weather", Arguments: `{"location":"NYC"}`, Status: v1.StatusCompleted}})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "resp_tc", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonToolCalls})

	allStr := string(allOut)
	if !strings.Contains(allStr, "get_weather") {
		t.Errorf("expected tool name 'get_weather'")
	}
	if !strings.Contains(allStr, "tool_calls") {
		t.Errorf("expected finish_reason tool_calls")
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("expected [DONE]")
	}
}

// CC-1: error event produces an error body + [DONE].
func TestCCNewFromCanonicalStream_ErrorEvent(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	feed := func(event string, data any) []byte {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		return out
	}
	var allOut []byte
	allOut = append(allOut, feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "r", Model: "m"})...)
	allOut = append(allOut, feed(v1.EventError, v1.ErrorEvent{Code: "server_error", Message: "boom"})...)

	allStr := string(allOut)
	if !strings.Contains(allStr, "boom") {
		t.Errorf("error message missing: %s", allStr)
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("[DONE] missing after error")
	}
}

func TestCCToCanonicalStream_OllamaReasoning(t *testing.T) {
	fn := (CCTranslator{}).NewToCanonicalStream()
	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":     "chatcmpl-rs",
			"object": "chat.completion.chunk",
			"model":  "gpt-oss",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "reasoning": "thinking..."},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":     "chatcmpl-rs",
			"object": "chat.completion.chunk",
			"model":  "gpt-oss",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": "Answer"},
			}},
		}),
		ccDoneChunk(),
	}
	var out []byte
	for _, c := range chunks {
		b, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		out = append(out, b...)
	}
	s := string(out)
	if !strings.Contains(s, `"reasoning"`) {
		t.Fatalf("expected a canonical reasoning delta in stream output:\n%s", s)
	}
	if !strings.Contains(s, `"cc_reasoning_field":"reasoning"`) {
		t.Fatalf("expected provider_data to preserve Ollama field:\n%s", s)
	}
}

// ccToolCallChunk builds one CC chunk carrying a single tool_calls delta.
func ccToolCallChunk(id, name, args string) []byte {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	tc := map[string]any{"index": 0, "type": "function", "function": fn}
	if id != "" {
		tc["id"] = id
	}
	return ccSSEChunk(map[string]any{
		"id":      "chatcmpl-tc",
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{tc}},
		}},
	})
}

// ccCompletedFunctionCall drains chunks and returns the first completed
// function_call item's name and status.
func ccCompletedFunctionCall(t *testing.T, chunks [][]byte) (name, status string) {
	t.Helper()
	fn := (CCTranslator{}).NewToCanonicalStream()
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		for _, frame := range splitCanonicalFrames(out) {
			ev, data, ok := v1.ParseSSEChunk(frame)
			if !ok || ev != v1.EventItemCompleted {
				continue
			}
			var raw struct {
				Item struct {
					Type   string `json:"type"`
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"item"`
			}
			_ = json.Unmarshal(data, &raw)
			if raw.Item.Type == string(v1.ItemTypeFunctionCall) {
				return raw.Item.Name, raw.Item.Status
			}
		}
	}
	t.Fatal("no completed function_call item in stream")
	return "", ""
}

func TestCCNewToCanonicalStream_ToolCall_TruncatedArgsIncomplete(t *testing.T) {
	_, status := ccCompletedFunctionCall(t, [][]byte{
		ccToolCallChunk("call_1", "search", `{"q":`),
		ccToolCallChunk("", "", `"unter`),
		ccDoneChunk(),
	})
	if status != string(v1.StatusIncomplete) {
		t.Errorf("truncated args: status got %q want incomplete", status)
	}

	_, status = ccCompletedFunctionCall(t, [][]byte{
		ccToolCallChunk("call_1", "search", `{"q":`),
		ccToolCallChunk("", "", `"ok"}`),
		ccDoneChunk(),
	})
	if status != string(v1.StatusCompleted) {
		t.Errorf("valid args: status got %q want completed", status)
	}
}

// Some OpenAI-compatible upstreams send the id-only fragment first and the
// function name in a later one; the completed item must still carry it.
func TestCCNewToCanonicalStream_ToolCall_NameBackfill(t *testing.T) {
	name, status := ccCompletedFunctionCall(t, [][]byte{
		ccToolCallChunk("call_late", "", ""),
		ccToolCallChunk("", "search", `{"q":"hi"}`),
		ccDoneChunk(),
	})
	if name != "search" {
		t.Errorf("name: got %q want search", name)
	}
	if status != string(v1.StatusCompleted) {
		t.Errorf("status: got %q want completed", status)
	}
}
