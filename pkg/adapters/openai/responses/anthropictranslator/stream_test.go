package anthropictranslator

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// sseChunk formats a raw Anthropic SSE chunk from event name + data map.
func sseChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

// collectEvents runs a sequence of chunks through the stream and collects
// all emitted event names.
func collectEvents(t *testing.T, chunks [][]byte) []string {
	t.Helper()
	s := NewStream(nil)
	var names []string
	for _, c := range chunks {
		frames, err := s.Translate(c)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		for _, f := range frames {
			names = append(names, f.Event)
		}
	}
	return names
}

// decodeFrame decodes the Data field of an SSEFrame into a map.
func decodeFrame(t *testing.T, f SSEFrame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.Data, &m); err != nil {
		t.Fatalf("decode frame %s: %v", f.Event, err)
	}
	return m
}

// ---- stream helpers ----

func messageStartChunk(id, model string) []byte {
	return sseChunk("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    id,
			"type":  "message",
			"role":  "assistant",
			"model": model,
			"content": []any{},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 0,
			},
		},
	})
}

func contentBlockStartText(index int) []byte {
	return sseChunk("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

func contentBlockStartToolUse(index int, id, name string) []byte {
	return sseChunk("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   id,
			"name": name,
		},
	})
}

func contentBlockStartThinking(index int) []byte {
	return sseChunk("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "thinking",
		},
	})
}

func textDeltaChunk(index int, text string) []byte {
	return sseChunk("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
}

func inputJSONDeltaChunk(index int, partial string) []byte {
	return sseChunk("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": partial,
		},
	})
}

func thinkingDeltaChunk(index int, text string) []byte {
	return sseChunk("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
}

func contentBlockStopChunk(index int) []byte {
	return sseChunk("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
}

func messageDeltaChunk(stopReason string, outputTokens int) []byte {
	return sseChunk("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason,
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
		},
	})
}

func messageStopChunk() []byte {
	return sseChunk("message_stop", map[string]any{"type": "message_stop"})
}

// ---- tests ----

func TestStream_SimpleText(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_01", "claude-opus-4-5"),
		contentBlockStartText(0),
		textDeltaChunk(0, "Hello, "),
		textDeltaChunk(0, "world!"),
		textDeltaChunk(0, " How are you?"),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 20),
		messageStopChunk(),
	}

	s := NewStream(nil)
	var allFrames []SSEFrame
	for _, c := range chunks {
		frames, err := s.Translate(c)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		allFrames = append(allFrames, frames...)
	}

	events := make([]string, len(allFrames))
	for i, f := range allFrames {
		events[i] = f.Event
	}

	// Expected sequence
	want := []string{
		responses.EventCreated,
		responses.EventInProgress,
		responses.EventOutputItemAdded,
		responses.EventContentPartAdded,
		responses.EventOutputTextDelta,
		responses.EventOutputTextDelta,
		responses.EventOutputTextDelta,
		responses.EventOutputTextDone,
		responses.EventContentPartDone,
		responses.EventOutputItemDone,
		responses.EventCompleted,
	}
	if len(events) != len(want) {
		t.Fatalf("events: got %v want %v", events, want)
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("events[%d]: got %q want %q", i, e, want[i])
		}
	}

	// Verify the completed event has the assembled text.
	lastFrame := allFrames[len(allFrames)-1]
	m := decodeFrame(t, lastFrame)
	resp := m["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("status: got %q", resp["status"])
	}
}

func TestStream_ToolUse(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_02", "claude-3-5-sonnet-20241022"),
		contentBlockStartToolUse(0, "toolu_01", "search"),
		inputJSONDeltaChunk(0, `{"q`),
		inputJSONDeltaChunk(0, `":"golang"}`),
		contentBlockStopChunk(0),
		messageDeltaChunk("tool_use", 30),
		messageStopChunk(),
	}

	s := NewStream(nil)
	var allFrames []SSEFrame
	for _, c := range chunks {
		frames, err := s.Translate(c)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		allFrames = append(allFrames, frames...)
	}

	events := make([]string, len(allFrames))
	for i, f := range allFrames {
		events[i] = f.Event
	}

	want := []string{
		responses.EventCreated,
		responses.EventInProgress,
		responses.EventOutputItemAdded,
		responses.EventFunctionCallArgumentsDelta,
		responses.EventFunctionCallArgumentsDelta,
		responses.EventFunctionCallArgumentsDone,
		responses.EventOutputItemDone,
		responses.EventCompleted,
	}
	if len(events) != len(want) {
		t.Fatalf("events: got %v want %v", events, want)
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("events[%d]: got %q want %q", i, e, want[i])
		}
	}

	// Verify completed response finish_reason is tool_calls.
	lastFrame := allFrames[len(allFrames)-1]
	m := decodeFrame(t, lastFrame)
	resp := m["response"].(map[string]any)
	if resp["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason: got %q", resp["finish_reason"])
	}
}

func TestStream_ThinkingThenText(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_03", "claude-opus-4-5"),
		contentBlockStartThinking(0),
		thinkingDeltaChunk(0, "Let me think..."),
		contentBlockStopChunk(0),
		contentBlockStartText(1),
		textDeltaChunk(1, "The answer is 42."),
		contentBlockStopChunk(1),
		messageDeltaChunk("end_turn", 40),
		messageStopChunk(),
	}

	s := NewStream(nil)
	var allFrames []SSEFrame
	for _, c := range chunks {
		frames, err := s.Translate(c)
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		allFrames = append(allFrames, frames...)
	}

	events := make([]string, len(allFrames))
	for i, f := range allFrames {
		events[i] = f.Event
	}

	// Should see thinking item sequence, then text item sequence.
	want := []string{
		responses.EventCreated,
		responses.EventInProgress,
		// thinking block
		responses.EventOutputItemAdded,
		responses.EventContentPartAdded,
		responses.EventReasoningTextDelta,
		responses.EventReasoningTextDone,
		responses.EventContentPartDone,
		responses.EventOutputItemDone,
		// text block
		responses.EventOutputItemAdded,
		responses.EventContentPartAdded,
		responses.EventOutputTextDelta,
		responses.EventOutputTextDone,
		responses.EventContentPartDone,
		responses.EventOutputItemDone,
		// completed
		responses.EventCompleted,
	}
	if len(events) != len(want) {
		t.Fatalf("events:\n  got  %v\n  want %v", events, want)
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("events[%d]: got %q want %q", i, e, want[i])
		}
	}

	// Final response should have 2 output items: reasoning + message.
	lastFrame := allFrames[len(allFrames)-1]
	m := decodeFrame(t, lastFrame)
	resp := m["response"].(map[string]any)
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Errorf("output len: got %d want 2", len(output))
	}
}

func TestStream_PingIgnored(t *testing.T) {
	ping := sseChunk("ping", map[string]any{"type": "ping"})
	s := NewStream(nil)
	frames, err := s.Translate(ping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("ping should produce no frames, got %d", len(frames))
	}
}

func TestStream_ErrorChunk(t *testing.T) {
	errChunk := sseChunk("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "overloaded_error",
			"message": "Anthropic is overloaded",
		},
	})
	s := NewStream(nil)
	frames, err := s.Translate(errChunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Event != responses.EventError {
		t.Errorf("event: got %q want %q", frames[0].Event, responses.EventError)
	}
	m := decodeFrame(t, frames[0])
	if m["message"] != "Anthropic is overloaded" {
		t.Errorf("error message: got %q", m["message"])
	}
}

func TestStream_Bytes(t *testing.T) {
	f := SSEFrame{Event: "response.created", Data: []byte(`{"response":{}}`)}
	b := f.Bytes()
	got := string(b)
	want := "event: response.created\ndata: {\"response\":{}}\n\n"
	if got != want {
		t.Errorf("Bytes: got %q want %q", got, want)
	}
}

func TestStream_MaxTokensIncomplete(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_04", "claude-opus-4-5"),
		contentBlockStartText(0),
		textDeltaChunk(0, "Partial..."),
		contentBlockStopChunk(0),
		messageDeltaChunk("max_tokens", 100),
		messageStopChunk(),
	}

	events := collectEvents(t, chunks)
	last := events[len(events)-1]
	if last != responses.EventIncomplete {
		t.Errorf("last event: got %q want %q", last, responses.EventIncomplete)
	}
}
