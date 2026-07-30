package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- Stream: canonical → Anthropic tests ----

func canonSSEFrame(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return v1.SSEFrame{Event: event, Data: b}.Bytes()
}

func collectAnthropicEvents(t *testing.T, chunks [][]byte) []string {
	t.Helper()
	fn := (AnthropicTranslator{}).NewFromCanonicalStream()
	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		frames := splitFrames(out)
		for _, f := range frames {
			ev, _, ok := parseAnthropicSSEChunk(f)
			if ok && ev != "" {
				events = append(events, ev)
			}
		}
	}
	return events
}

func parseAnthropicSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	lines := strings.Split(string(chunk), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			data = []byte(strings.TrimSpace(line[5:]))
		}
	}
	return event, data, len(data) > 0
}

func TestCanonicalToAnthropic_TextStream(t *testing.T) {
	chunks := [][]byte{
		canonSSEFrame(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "msg_001", Model: "claude-3-5-sonnet-20241022"}),
		canonSSEFrame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0}),
		canonSSEFrame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello"}),
		canonSSEFrame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Index: 0, Item: &v1.Message{
			ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: "Hello"}},
		}}),
		canonSSEFrame(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID:           "msg_001",
			Status:       v1.StatusCompleted,
			FinishReason: v1.FinishReasonStop,
			Usage:        usage.Tokens{"input": 5, "output": 5},
		}),
	}

	events := collectAnthropicEvents(t, chunks)
	// Expected: message_start, ping, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	first := events[0]
	if first != "message_start" {
		t.Errorf("first event: %q want message_start", first)
	}
	last := events[len(events)-1]
	if last != "message_stop" {
		t.Errorf("last event: %q want message_stop", last)
	}
}

func TestCanonicalToAnthropic_IndexGapKeepsDeltasOnOpenedBlock(t *testing.T) {
	chunks := [][]byte{
		canonSSEFrame(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "msg_gap", Model: "claude-3-5-sonnet-20241022"}),
		canonSSEFrame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0}),
		canonSSEFrame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "first"}),
		canonSSEFrame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Index: 0, Item: &v1.Message{
			ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: "first"}},
		}}),
		canonSSEFrame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "call_2", ItemType: v1.ItemTypeFunctionCall, Index: 2, Name: "search"}),
		canonSSEFrame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "call_2", Index: 2, Kind: v1.DeltaKindArguments, Delta: `{"q":"relay"}`}),
		canonSSEFrame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "call_2", Index: 2, Item: &v1.FunctionCall{
			ID: "call_2", CallID: "call_2", Name: "search", Arguments: `{"q":"relay"}`, Status: v1.StatusCompleted,
		}}),
		canonSSEFrame(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID:           "msg_gap",
			Status:       v1.StatusCompleted,
			FinishReason: v1.FinishReasonToolCalls,
		}),
	}

	fn := (AnthropicTranslator{}).NewFromCanonicalStream()
	var blockStarts, blockDeltas, blockStops []int
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		for _, f := range splitFrames(out) {
			ev, data, ok := parseAnthropicSSEChunk(f)
			if !ok {
				continue
			}
			var frame struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("decode anthropic frame %q: %v", data, err)
			}
			switch ev {
			case "content_block_start":
				blockStarts = append(blockStarts, frame.Index)
			case "content_block_delta":
				blockDeltas = append(blockDeltas, frame.Index)
			case "content_block_stop":
				blockStops = append(blockStops, frame.Index)
			}
		}
	}

	want := []int{0, 1}
	if !intSlicesEqual(blockStarts, want) {
		t.Fatalf("content_block_start indices: got %v want %v", blockStarts, want)
	}
	if !intSlicesEqual(blockDeltas, want) {
		t.Fatalf("content_block_delta indices: got %v want %v", blockDeltas, want)
	}
	if !intSlicesEqual(blockStops, want) {
		t.Fatalf("content_block_stop indices: got %v want %v", blockStops, want)
	}
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
