package cctranslator

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// decodeCompletedEvent decodes the JSON of a response.completed SSEFrame into
// a *responses.Response using the package's canonical UnmarshalResponse so that
// the polymorphic Output []Item is decoded correctly.
func decodeCompletedEvent(t *testing.T, frame SSEFrame) *responses.Response {
	t.Helper()
	var wrapper struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(frame.Data, &wrapper); err != nil {
		t.Fatalf("decode completed wrapper: %v — data: %s", err, frame.Data)
	}
	resp, err := responses.UnmarshalResponse(wrapper.Response)
	if err != nil {
		t.Fatalf("UnmarshalResponse: %v", err)
	}
	return resp
}

// decodeOutputItemDone decodes an output_item.done frame and returns the raw
// item JSON so callers can probe it with a type probe.
func decodeOutputItemDone(t *testing.T, frame SSEFrame) (outputIndex int, itemType string, itemJSON json.RawMessage) {
	t.Helper()
	var raw struct {
		OutputIndex int             `json:"output_index"`
		Item        json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(frame.Data, &raw); err != nil {
		t.Fatalf("decode output_item.done: %v", err)
	}
	var typeProbe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw.Item, &typeProbe)
	return raw.OutputIndex, typeProbe.Type, raw.Item
}

// decodeItemAdded decodes an output_item.added frame and returns the item type string.
func decodeItemAdded(t *testing.T, frame SSEFrame) (outputIndex int, itemType string) {
	t.Helper()
	var raw struct {
		OutputIndex int             `json:"output_index"`
		Item        json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(frame.Data, &raw); err != nil {
		t.Fatalf("decode output_item.added: %v", err)
	}
	var typeProbe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw.Item, &typeProbe)
	return raw.OutputIndex, typeProbe.Type
}

// ---- helpers ----

// makeChunk builds a minimal CC SSE chunk.
func makeChunk(id, model string, content *string, toolCalls []map[string]any, finishReason *string, usage map[string]any) []byte {
	delta := map[string]any{}
	if content != nil {
		delta["content"] = *content
	}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		obj["usage"] = usage
	}
	b, _ := json.Marshal(obj)
	return append([]byte("data: "), append(b, []byte("\n\n")...)...)
}

func makeDoneChunk() []byte {
	return []byte("data: [DONE]\n\n")
}

// collectEvents calls s.Translate for each chunk and returns all SSEFrames.
func collectEvents(t *testing.T, s *Stream, chunks [][]byte) []SSEFrame {
	t.Helper()
	var all []SSEFrame
	for _, c := range chunks {
		frames, err := s.Translate(c)
		if err != nil {
			t.Fatalf("Translate error: %v", err)
		}
		all = append(all, frames...)
	}
	return all
}

// eventNames returns the Event field for each frame.
func eventNames(frames []SSEFrame) []string {
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.Event
	}
	return names
}

func strOf(s string) *string { return &s }

// ---- tests ----

func TestStream_SimpleText(t *testing.T) {
	s := NewStream(nil)
	chunks := [][]byte{
		makeChunk("id1", "gpt-4o", strOf("Hello"), nil, nil, nil),
		makeChunk("id1", "gpt-4o", strOf(", world"), nil, nil, nil),
		makeChunk("id1", "gpt-4o", strOf("!"), nil, strOf("stop"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)
	names := eventNames(frames)

	// Expected sequence:
	// response.created, response.in_progress
	// response.output_item.added (message)
	// response.content_part.added
	// response.output_text.delta × 3
	// response.output_text.done
	// response.content_part.done
	// response.output_item.done
	// response.completed
	expected := []string{
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
	if len(names) != len(expected) {
		t.Fatalf("event count: got %d, want %d\ngot:  %v\nwant: %v", len(names), len(expected), names, expected)
	}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("event[%d]: got %q, want %q", i, names[i], want)
		}
	}

	// Verify the completed event has the full text.
	lastFrame := frames[len(frames)-1]
	resp := decodeCompletedEvent(t, lastFrame)
	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d, want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] not *Message, got %T", resp.Output[0])
	}
	if len(msg.Content) == 0 {
		t.Fatal("message content is empty")
	}
	otp, ok := msg.Content[0].(*responses.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] not *OutputTextPart, got %T", msg.Content[0])
	}
	if otp.Text != "Hello, world!" {
		t.Errorf("accumulated text: got %q, want Hello, world!", otp.Text)
	}
}

func TestStream_ToolCall(t *testing.T) {
	s := NewStream(nil)
	// Chunk 1: tool_call with name; first args chunk.
	tc1 := []map[string]any{
		{"index": 0, "id": "call_abc", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"loc`}},
	}
	// Chunk 2: more args
	tc2 := []map[string]any{
		{"index": 0, "function": map[string]any{"arguments": `ation`}},
	}
	// Chunk 3: finish args
	tc3 := []map[string]any{
		{"index": 0, "function": map[string]any{"arguments": `":"NYC"}`}},
	}
	chunks := [][]byte{
		makeChunk("id2", "gpt-4o", nil, tc1, nil, nil),
		makeChunk("id2", "gpt-4o", nil, tc2, nil, nil),
		makeChunk("id2", "gpt-4o", nil, tc3, strOf("tool_calls"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)
	names := eventNames(frames)

	// Expected:
	// created, in_progress
	// output_item.added (function_call)
	// function_call_arguments.delta × 3
	// function_call_arguments.done
	// output_item.done
	// completed
	expected := []string{
		responses.EventCreated,
		responses.EventInProgress,
		responses.EventOutputItemAdded,
		responses.EventFunctionCallArgumentsDelta,
		responses.EventFunctionCallArgumentsDelta,
		responses.EventFunctionCallArgumentsDelta,
		responses.EventFunctionCallArgumentsDone,
		responses.EventOutputItemDone,
		responses.EventCompleted,
	}
	if len(names) != len(expected) {
		t.Fatalf("event count: got %d, want %d\ngot:  %v\nwant: %v", len(names), len(expected), names, expected)
	}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("event[%d]: got %q, want %q", i, names[i], want)
		}
	}

	// Verify accumulated arguments in done event.
	doneIdx := -1
	for i, f := range frames {
		if f.Event == responses.EventFunctionCallArgumentsDone {
			doneIdx = i
			break
		}
	}
	if doneIdx == -1 {
		t.Fatal("no function_call_arguments.done event found")
	}
	var doneEvt responses.FunctionCallArgumentsDoneEvent
	if err := json.Unmarshal(frames[doneIdx].Data, &doneEvt); err != nil {
		t.Fatalf("parse done event: %v", err)
	}
	if doneEvt.Arguments != `{"location":"NYC"}` {
		t.Errorf("accumulated args: got %q", doneEvt.Arguments)
	}
	if doneEvt.CallID != "call_abc" {
		t.Errorf("call_id: got %q, want call_abc", doneEvt.CallID)
	}

	// Verify output_item.done carries the function_call item type.
	itemDoneIdx := -1
	for i, f := range frames {
		if f.Event == responses.EventOutputItemDone {
			itemDoneIdx = i
			break
		}
	}
	_, itemType, itemRaw := decodeOutputItemDone(t, frames[itemDoneIdx])
	if itemType != "function_call" {
		t.Errorf("item type: got %q, want function_call", itemType)
	}
	var fc struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(itemRaw, &fc)
	if fc.Name != "get_weather" {
		t.Errorf("function name: got %q", fc.Name)
	}
}

func TestStream_TextThenToolCall(t *testing.T) {
	// Text chunk first, then tool_call chunk — message item should close before tool item opens.
	s := NewStream(nil)
	tc := []map[string]any{
		{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "fn", "arguments": `{}`}},
	}
	chunks := [][]byte{
		makeChunk("id3", "gpt-4o", strOf("I'll call a tool."), nil, nil, nil),
		makeChunk("id3", "gpt-4o", nil, tc, strOf("tool_calls"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)

	// Text events should appear before tool events.
	sawTextDelta := false
	sawTextDone := false
	sawToolDelta := false
	sawToolDone := false
	sawItemDoneMsg := false
	sawItemDoneFC := false

	for _, f := range frames {
		switch f.Event {
		case responses.EventOutputTextDelta:
			if sawToolDelta {
				t.Error("output_text.delta appeared after function_call_arguments.delta")
			}
			sawTextDelta = true
		case responses.EventOutputTextDone:
			sawTextDone = true
		case responses.EventFunctionCallArgumentsDelta:
			sawToolDelta = true
		case responses.EventFunctionCallArgumentsDone:
			sawToolDone = true
		case responses.EventOutputItemDone:
			_, itemType, _ := decodeOutputItemDone(t, f)
			switch itemType {
			case "message":
				if sawItemDoneFC {
					t.Error("message item.done appeared after function_call item.done")
				}
				sawItemDoneMsg = true
			case "function_call":
				sawItemDoneFC = true
			}
		}
	}

	if !sawTextDelta {
		t.Error("missing output_text.delta")
	}
	if !sawTextDone {
		t.Error("missing output_text.done")
	}
	if !sawToolDelta {
		t.Error("missing function_call_arguments.delta")
	}
	if !sawToolDone {
		t.Error("missing function_call_arguments.done")
	}
	if !sawItemDoneMsg {
		t.Error("missing message output_item.done")
	}
	if !sawItemDoneFC {
		t.Error("missing function_call output_item.done")
	}

	// Total item.done count should be 2.
	doneCnt := 0
	for _, f := range frames {
		if f.Event == responses.EventOutputItemDone {
			doneCnt++
		}
	}
	if doneCnt != 2 {
		t.Errorf("output_item.done count: got %d, want 2", doneCnt)
	}
}

func TestStream_ReasoningThenText(t *testing.T) {
	// Reasoning chunk first, then text.
	s := NewStream(nil)

	// Reasoning chunk: non-standard delta.reasoning_content field.
	reasoningChunk := []byte(fmt.Sprintf(`data: {"id":"id4","object":"chat.completion.chunk","created":1700000000,"model":"o1","choices":[{"index":0,"delta":{"reasoning_content":"let me think"},"finish_reason":null}]}`))
	reasoningChunk = append(reasoningChunk, []byte("\n\n")...)

	chunks := [][]byte{
		reasoningChunk,
		makeChunk("id4", "o1", strOf("The answer is 42."), nil, strOf("stop"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)

	sawReasoning := false
	sawText := false
	reasoningBeforeText := true
	seenText := false

	for _, f := range frames {
		switch f.Event {
		case responses.EventReasoningTextDelta:
			if seenText {
				reasoningBeforeText = false
			}
			sawReasoning = true
		case responses.EventOutputTextDelta:
			seenText = true
			sawText = true
		}
	}

	if !sawReasoning {
		t.Error("missing reasoning_text.delta")
	}
	if !sawText {
		t.Error("missing output_text.delta")
	}
	if !reasoningBeforeText {
		t.Error("reasoning delta appeared after text delta")
	}

	// Verify output_item.added for reasoning appears before output_item.added for message.
	var addedEvents []string
	for _, f := range frames {
		if f.Event == responses.EventOutputItemAdded {
			_, itemType := decodeItemAdded(t, f)
			addedEvents = append(addedEvents, itemType)
		}
	}
	if len(addedEvents) != 2 {
		t.Fatalf("output_item.added count: got %d, want 2; events: %v", len(addedEvents), addedEvents)
	}
	if addedEvents[0] != "reasoning" {
		t.Errorf("first added item: got %q, want reasoning", addedEvents[0])
	}
	if addedEvents[1] != "message" {
		t.Errorf("second added item: got %q, want message", addedEvents[1])
	}
}

func TestStream_UsageInFinalResponse(t *testing.T) {
	s := NewStream(nil)
	usageMap := map[string]any{
		"prompt_tokens":     20,
		"completion_tokens": 10,
		"total_tokens":      30,
	}
	chunks := [][]byte{
		makeChunk("id5", "gpt-4o", strOf("hi"), nil, strOf("stop"), usageMap),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)

	// Find completed event.
	var completedFrame *SSEFrame
	for i := range frames {
		if frames[i].Event == responses.EventCompleted {
			completedFrame = &frames[i]
			break
		}
	}
	if completedFrame == nil {
		t.Fatal("no completed event")
	}
	resp := decodeCompletedEvent(t, *completedFrame)
	if resp.Usage == nil {
		t.Fatal("usage is nil in completed event")
	}
	if resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 10 || resp.Usage.TotalTokens != 30 {
		t.Errorf("usage: got %+v", resp.Usage)
	}
}

func TestStream_EmptyContent_Skipped(t *testing.T) {
	// Empty string content should not open a message item.
	s := NewStream(nil)
	empty := ""
	chunks := [][]byte{
		makeChunk("id6", "gpt-4o", &empty, nil, strOf("stop"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)

	for _, f := range frames {
		if f.Event == responses.EventOutputItemAdded {
			t.Error("output_item.added should not be emitted for empty content")
		}
	}
}

func TestStream_MultipleToolCalls(t *testing.T) {
	// Two simultaneous tool calls (different indices in same chunk).
	s := NewStream(nil)
	tc := []map[string]any{
		{"index": 0, "id": "call_A", "type": "function", "function": map[string]any{"name": "fnA", "arguments": `{"a":1}`}},
		{"index": 1, "id": "call_B", "type": "function", "function": map[string]any{"name": "fnB", "arguments": `{"b":2}`}},
	}
	chunks := [][]byte{
		makeChunk("id7", "gpt-4o", nil, tc, strOf("tool_calls"), nil),
		makeDoneChunk(),
	}
	frames := collectEvents(t, s, chunks)

	// Should see two output_item.added events and two output_item.done events.
	addedCnt := 0
	doneCnt := 0
	for _, f := range frames {
		switch f.Event {
		case responses.EventOutputItemAdded:
			addedCnt++
		case responses.EventOutputItemDone:
			doneCnt++
		}
	}
	if addedCnt != 2 {
		t.Errorf("output_item.added count: got %d, want 2", addedCnt)
	}
	if doneCnt != 2 {
		t.Errorf("output_item.done count: got %d, want 2", doneCnt)
	}

	// Completed response should contain two function_call items.
	var completedFrame *SSEFrame
	for i := range frames {
		if frames[i].Event == responses.EventCompleted {
			completedFrame = &frames[i]
		}
	}
	if completedFrame == nil {
		t.Fatal("no completed event")
	}
	resp := decodeCompletedEvent(t, *completedFrame)
	if len(resp.Output) != 2 {
		t.Errorf("output len: got %d, want 2", len(resp.Output))
	}
}
