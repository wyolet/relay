package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func responsesSSEChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func TestResponsesNewToCanonicalStream_TextSequence(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		responsesSSEChunk(ResponsesEventCreated, ResponsesCreatedEvent{
			Response: &ResponsesResponse{
				ID:     "resp_s1",
				Object: "response",
				Model:  "gpt-5",
				Status: ResponsesStatusInProgress,
				Output: []ResponsesItem{},
			},
		}),
		responsesSSEChunk(ResponsesEventOutputItemAdded, ResponsesItemAddedEvent{
			OutputIndex: 0,
			Item:        &ResponsesMessage{ID: "msg_0", Role: ResponsesRoleAssistant, Status: ResponsesStatusInProgress},
		}),
		responsesSSEChunk(ResponsesEventOutputTextDelta, ResponsesOutputTextDeltaEvent{
			ItemID: "msg_0", OutputIndex: 0, ContentIndex: 0, Delta: "Hello",
		}),
		responsesSSEChunk(ResponsesEventOutputTextDelta, ResponsesOutputTextDeltaEvent{
			ItemID: "msg_0", OutputIndex: 0, ContentIndex: 0, Delta: " world",
		}),
		responsesSSEChunk(ResponsesEventOutputItemDone, ResponsesOutputItemDoneEvent{
			OutputIndex: 0,
			Item: &ResponsesMessage{
				ID:      "msg_0",
				Role:    ResponsesRoleAssistant,
				Status:  ResponsesStatusCompleted,
				Content: []ResponsesPart{&ResponsesOutputTextPart{Text: "Hello world"}},
			},
		}),
		responsesSSEChunk(ResponsesEventCompleted, ResponsesCompletedEvent{
			Response: &ResponsesResponse{
				ID:     "resp_s1",
				Object: "response",
				Model:  "gpt-5",
				Status: ResponsesStatusCompleted,
				Output: []ResponsesItem{},
			},
		}),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

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

func TestResponsesNewToCanonicalStream_FunctionCallDelta(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		responsesSSEChunk(ResponsesEventCreated, ResponsesCreatedEvent{
			Response: &ResponsesResponse{ID: "resp_fc", Model: "gpt-5", Status: ResponsesStatusInProgress, Output: []ResponsesItem{}},
		}),
		responsesSSEChunk(ResponsesEventOutputItemAdded, ResponsesItemAddedEvent{
			OutputIndex: 0,
			Item:        &ResponsesFunctionCall{ID: "fc_0", CallID: "call_abc", Name: "search", Status: ResponsesStatusInProgress},
		}),
		responsesSSEChunk(ResponsesEventFunctionCallArgumentsDelta, ResponsesFunctionCallArgumentsDeltaEvent{
			ItemID: "fc_0", OutputIndex: 0, CallID: "call_abc", Delta: `{"q":"golang"}`,
		}),
		responsesSSEChunk(ResponsesEventOutputItemDone, ResponsesOutputItemDoneEvent{
			OutputIndex: 0,
			Item: &ResponsesFunctionCall{
				ID: "fc_0", CallID: "call_abc", Name: "search",
				Arguments: `{"q":"golang"}`, Status: ResponsesStatusCompleted,
			},
		}),
		responsesSSEChunk(ResponsesEventCompleted, ResponsesCompletedEvent{
			Response: &ResponsesResponse{ID: "resp_fc", Model: "gpt-5", Status: ResponsesStatusCompleted, Output: []ResponsesItem{}},
		}),
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

func TestResponsesNewToCanonicalStream_UnknownEventsDropped(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewToCanonicalStream()

	// Unknown event type should produce no output without error.
	chunk := responsesSSEChunk("response.unknown_future_event", map[string]any{"type": "unknown"})
	out, err := fn(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := extractCanonicalEvents(out)
	if len(events) != 0 {
		t.Errorf("expected no events for unknown type, got %v", events)
	}
}

// --- NewFromCanonicalStream (canonical → Responses) ---

func canonicalChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func TestResponsesNewFromCanonicalStream_TextSequence(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewFromCanonicalStream()
	if fn == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}

	chunks := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{
			ID: "resp_s1", Model: "gpt-5",
		}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{
			ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello",
		}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{
			ItemID: "msg_0",
			Index:  0,
			Item: &v1.Message{
				ID:      "msg_0",
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Hello"}},
			},
		}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID:           "resp_s1",
			Status:       v1.StatusCompleted,
			FinishReason: v1.FinishReasonStop,
			Usage:        usage.Tokens{"input": 5, "output": 3},
		}),
	}

	var responsesEvents []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		responsesEvents = append(responsesEvents, extractResponsesEvents(out)...)
	}

	wantContains := []string{
		ResponsesEventCreated,
		ResponsesEventOutputItemAdded,
		ResponsesEventOutputTextDelta,
		ResponsesEventOutputItemDone,
		ResponsesEventCompleted,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range responsesEvents {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in %v", want, responsesEvents)
		}
	}
}

func TestResponsesNewFromCanonicalStream_FunctionCall(t *testing.T) {
	tr := ResponsesTranslator{}
	fn := tr.NewFromCanonicalStream()

	chunks := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_fc", Model: "gpt-5"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{
			ItemID: "fc_0", ItemType: v1.ItemTypeFunctionCall, Index: 0,
		}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{
			ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"q":"golang"}`,
		}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{
			ItemID: "fc_0",
			Index:  0,
			Item: &v1.FunctionCall{
				ID:        "fc_0",
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
				Status:    v1.StatusCompleted,
			},
		}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID: "resp_fc", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonToolCalls,
		}),
	}

	var responsesEvents []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		responsesEvents = append(responsesEvents, extractResponsesEvents(out)...)
	}

	wantContains := []string{
		ResponsesEventFunctionCallArgumentsDelta,
		ResponsesEventFunctionCallArgumentsDone,
		ResponsesEventOutputItemDone,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range responsesEvents {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in %v", want, responsesEvents)
		}
	}
}

// extractResponsesEvents parses concatenated Responses SSE bytes and collects event names.
func extractResponsesEvents(b []byte) []string {
	var names []string
	for _, frame := range splitCanonicalFrames(b) {
		event, _, ok := ParseResponsesSSEChunk(frame)
		if ok && event != "" {
			names = append(names, event)
		}
	}
	return names
}

// --- E2E: Responses → canonical → CC wire → canonical → Responses ---

// R-2: response.refusal.delta events map to text item.delta in canonical stream.
func TestResponsesNewToCanonicalStream_RefusalDelta(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(responsesSSEChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(ResponsesEventCreated, ResponsesCreatedEvent{Response: &ResponsesResponse{
		ID: "resp_ref", Model: "gpt-4o", Status: ResponsesStatusInProgress,
	}})
	feed(ResponsesEventOutputItemAdded, map[string]any{
		"output_index": 0,
		"item":         map[string]any{"type": "message", "id": "msg_0", "role": "assistant"},
	})
	feed(ResponsesEventRefusalDelta, ResponsesRefusalDeltaEvent{
		ItemID: "msg_0", OutputIndex: 0, Delta: "I cannot help with that",
	})
	feed(ResponsesEventRefusalDone, ResponsesRefusalDoneEvent{
		ItemID: "msg_0", OutputIndex: 0, Refusal: "I cannot help with that",
	})
	feed(ResponsesEventCompleted, ResponsesCompletedEvent{Response: &ResponsesResponse{
		ID: "resp_ref", Model: "gpt-4o", Status: ResponsesStatusCompleted,
	}})

	events := extractCanonicalEvents(allOut)
	hasDelta := false
	for _, e := range events {
		if e == v1.EventItemDelta {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Errorf("expected item.delta for refusal text, got: %v", events)
	}
	if !strings.Contains(string(allOut), "I cannot help with that") {
		t.Errorf("refusal text missing from output")
	}
}

// R-2: response.failed must emit generation.completed so the consumer isn't hung.
func TestResponsesNewToCanonicalStream_ResponseFailed(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()

	feed := func(event string, data any) []byte {
		out, err := fn(responsesSSEChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		return out
	}

	var allOut []byte
	allOut = append(allOut, feed(ResponsesEventCreated, ResponsesCreatedEvent{Response: &ResponsesResponse{
		ID: "resp_fail", Model: "gpt-4o", Status: ResponsesStatusInProgress,
	}})...)
	allOut = append(allOut, feed(ResponsesEventFailed, ResponsesFailedEvent{Response: &ResponsesResponse{
		ID: "resp_fail", Model: "gpt-4o", Status: ResponsesStatusFailed,
	}})...)

	events := extractCanonicalEvents(allOut)
	hasCompleted := false
	for _, e := range events {
		if e == v1.EventGenerationCompleted {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Errorf("expected generation.completed after response.failed, got: %v", events)
	}
}

// R-3: canonical→Responses streaming emits non-empty call_id and name on function call events.
func TestResponsesNewFromCanonicalStream_FunctionCallHasNameAndCallID(t *testing.T) {
	fn := (ResponsesTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_r3", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_r3", ItemType: v1.ItemTypeFunctionCall, Index: 0, Name: "search"})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_r3", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"q":"go"}`})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{
		ItemID: "fc_r3",
		Index:  0,
		Item: &v1.FunctionCall{
			ID:        "fc_r3",
			CallID:    "call_r3",
			Name:      "search",
			Arguments: `{"q":"go"}`,
			Status:    v1.StatusCompleted,
		},
	})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
		ID:           "resp_r3",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
	})

	allStr := string(allOut)
	if !strings.Contains(allStr, "search") {
		t.Errorf("function name 'search' missing from output")
	}
	// Arguments delta events must carry a non-empty call_id.
	var foundArgsDelta bool
	for _, raw := range splitSSEFrames(allOut) {
		evtName, data, ok := ParseResponsesSSEChunk(raw)
		if !ok || evtName != ResponsesEventFunctionCallArgumentsDelta {
			continue
		}
		foundArgsDelta = true
		var delta ResponsesFunctionCallArgumentsDeltaEvent
		if err := json.Unmarshal(data, &delta); err != nil {
			t.Fatalf("unmarshal delta: %v", err)
		}
		if delta.CallID == "" {
			t.Errorf("call_id must not be empty in function_call_arguments.delta")
		}
	}
	if !foundArgsDelta {
		t.Errorf("no function_call_arguments.delta event found")
	}
}

// TestResponsesStream_HostedToolNoOrphanStarted: output_item.added for a
// hosted-tool type must not emit a canonical item.started (which would orphan a
// started-without-completed, since the item's done is dropped).
func TestResponsesStream_HostedToolNoOrphanStarted(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()
	chunk := responsesSSEChunk(ResponsesEventOutputItemAdded, map[string]any{
		"output_index": 0,
		"item":         map[string]any{"type": "web_search_call", "id": "ws_0", "status": "in_progress"},
	})
	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range extractCanonicalEvents(out) {
		if e == v1.EventItemStarted {
			t.Fatalf("emitted item.started for a hosted-tool item: %q", out)
		}
	}
}

// --- tool_choice object modes (PR3) ---

// TestResponsesStream_FunctionCallItemStartedCarriesName: item.started for a
// function_call must carry the tool name (for downstream Anthropic-style emit).
func TestResponsesStream_FunctionCallItemStartedCarriesName(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()
	chunk := responsesSSEChunk(ResponsesEventOutputItemAdded, map[string]any{
		"output_index": 0,
		"item":         map[string]any{"type": "function_call", "id": "fc_0", "call_id": "c0", "name": "search", "arguments": ""},
	})
	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	// Find the item.started frame and assert its name.
	for _, fr := range splitCanonicalFrames(out) {
		event, data, ok := v1.ParseSSEChunk(fr)
		if !ok || event != v1.EventItemStarted {
			continue
		}
		var ev v1.ItemStartedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Name != "search" {
			t.Errorf("item.started name: %q, want search", ev.Name)
		}
		return
	}
	t.Fatal("no item.started frame emitted")
}
