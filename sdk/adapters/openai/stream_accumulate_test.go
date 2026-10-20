package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// The stream translators assemble each item's closing text from its deltas.
// These tests stream many deltas and hold the assembled text to the plain
// concatenation of what was streamed.

// accumulateDeltas returns n text fragments mixing ASCII, JSON escapes and
// multi-byte runes, so an off-by-one or a lossy conversion shows.
func accumulateDeltas(n int) []string {
	out := make([]string, n)
	for i := range out {
		switch {
		case i%7 == 3:
			out[i] = ` "quoted" \ line` + "\n"
		case i%11 == 5:
			out[i] = " héllo ✓ 日本"
		default:
			out[i] = benchLongDeltaText(i)
		}
	}
	return out
}

// accumulateArgDeltas returns fragments that concatenate to the JSON object
// {"text": strings.Join(texts, "")}.
func accumulateArgDeltas(texts []string) []string {
	out := []string{`{"text":"`}
	for _, t := range texts {
		q, _ := json.Marshal(t)
		out = append(out, string(q[1:len(q)-1]))
	}
	return append(out, `"}`)
}

func streamFrames(t *testing.T, translate func([]byte) ([]byte, error), chunks [][]byte) [][]byte {
	t.Helper()
	var out []byte
	for i, c := range chunks {
		b, err := translate(c)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		out = append(out, b...)
	}
	return splitCanonicalFrames(out)
}

// completedCanonicalItem decodes the item of the only item.completed frame.
func completedCanonicalItem(t *testing.T, frames [][]byte) map[string]any {
	t.Helper()
	var item map[string]any
	for _, f := range frames {
		event, data, ok := v1.ParseSSEChunk(f)
		if !ok || event != v1.EventItemCompleted {
			continue
		}
		if item != nil {
			t.Fatal("more than one item.completed")
		}
		item = decodeMap(t, data)["item"].(map[string]any)
	}
	if item == nil {
		t.Fatal("no item.completed")
	}
	return item
}

func TestCCToCanonicalStream_AssemblesLongDeltas(t *testing.T) {
	texts := accumulateDeltas(2000)
	want := strings.Join(texts, "")
	ccChunk := func(delta string) []byte {
		return []byte(`data: {"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":` + delta + `}]}` + "\n\n")
	}
	stream := func(first string, delta func(string) string, fragments []string) [][]byte {
		chunks := [][]byte{ccChunk(first)}
		for _, f := range fragments {
			q, _ := json.Marshal(f)
			chunks = append(chunks, ccChunk(delta(string(q))))
		}
		return append(chunks, []byte("data: [DONE]\n\n"))
	}

	t.Run("text", func(t *testing.T) {
		frames := streamFrames(t, CCTranslator{}.NewToCanonicalStream(), stream(`{"role":"assistant"}`, func(q string) string {
			return `{"content":` + q + `}`
		}, texts))
		item := completedCanonicalItem(t, frames)
		if got := item["content"].([]any)[0].(map[string]any)["text"]; got != want {
			t.Fatalf("message text: got %d bytes, want %d", len(got.(string)), len(want))
		}
	})
	t.Run("reasoning", func(t *testing.T) {
		frames := streamFrames(t, CCTranslator{}.NewToCanonicalStream(), stream(`{"role":"assistant"}`, func(q string) string {
			return `{"reasoning_content":` + q + `}`
		}, texts))
		item := completedCanonicalItem(t, frames)
		if got := item["content"]; got != want {
			t.Fatalf("reasoning content differs from the streamed deltas")
		}
		if got := item["summary"].([]any)[0].(map[string]any)["text"]; got != want {
			t.Fatalf("reasoning summary differs from the streamed deltas")
		}
	})
	t.Run("tool_args", func(t *testing.T) {
		args := accumulateArgDeltas(texts)
		first := `{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":""}}]}`
		frames := streamFrames(t, CCTranslator{}.NewToCanonicalStream(), stream(first, func(q string) string {
			return `{"tool_calls":[{"index":0,"function":{"arguments":` + q + `}}]}`
		}, args))
		item := completedCanonicalItem(t, frames)
		if got := item["arguments"]; got != strings.Join(args, "") {
			t.Fatalf("arguments differ from the streamed deltas")
		}
		if item["status"] != string(v1.StatusCompleted) {
			t.Fatalf("status = %v, want completed for valid argument JSON", item["status"])
		}
	})
}

func TestResponsesFromCanonicalStream_AssemblesLongDeltas(t *testing.T) {
	texts := accumulateDeltas(2000)
	responsesFrame := func(t *testing.T, frames [][]byte, event string) map[string]any {
		t.Helper()
		for _, f := range frames {
			if ev, data, ok := v1.ParseSSEChunk(f); ok && ev == event {
				return decodeMap(t, data)
			}
		}
		t.Fatalf("no %s frame", event)
		return nil
	}
	// The completed items carry no text: the closing frames must come from the streamed deltas alone.
	stream := func(itemType v1.ItemType, kind v1.DeltaKind, fragments []string) [][]byte {
		return benchCanonicalStream(itemType, kind, len(fragments), func(i int) string { return fragments[i] })
	}

	t.Run("text", func(t *testing.T) {
		want := strings.Join(texts, "")
		frames := streamFrames(t, ResponsesTranslator{}.NewFromCanonicalStream(), stream(v1.ItemTypeMessage, v1.DeltaKindText, texts))
		if got := responsesFrame(t, frames, ResponsesEventOutputTextDone)["text"]; got != want {
			t.Fatal("output_text.done text differs from the streamed deltas")
		}
		if got := responsesFrame(t, frames, ResponsesEventContentPartDone)["part"].(map[string]any)["text"]; got != want {
			t.Fatal("content_part.done text differs from the streamed deltas")
		}
		item := responsesFrame(t, frames, ResponsesEventOutputItemDone)["item"].(map[string]any)
		if got := item["content"].([]any)[0].(map[string]any)["text"]; got != want {
			t.Fatal("output_item.done text differs from the streamed deltas")
		}
	})
	t.Run("reasoning", func(t *testing.T) {
		want := strings.Join(texts, "")
		frames := streamFrames(t, ResponsesTranslator{}.NewFromCanonicalStream(), stream(v1.ItemTypeReasoning, v1.DeltaKindReasoning, texts))
		if got := responsesFrame(t, frames, ResponsesEventReasoningTextDone)["text"]; got != want {
			t.Fatal("reasoning_text.done text differs from the streamed deltas")
		}
		item := responsesFrame(t, frames, ResponsesEventOutputItemDone)["item"].(map[string]any)
		if got := item["summary"].([]any)[0].(map[string]any)["text"]; got != want {
			t.Fatal("output_item.done summary differs from the streamed deltas")
		}
	})
	t.Run("tool_args", func(t *testing.T) {
		args := accumulateArgDeltas(texts)
		want := strings.Join(args, "")
		frames := streamFrames(t, ResponsesTranslator{}.NewFromCanonicalStream(), stream(v1.ItemTypeFunctionCall, v1.DeltaKindArguments, args))
		if got := responsesFrame(t, frames, ResponsesEventFunctionCallArgumentsDone)["arguments"]; got != want {
			t.Fatal("function_call_arguments.done differs from the streamed deltas")
		}
		item := responsesFrame(t, frames, ResponsesEventOutputItemDone)["item"].(map[string]any)
		if got := item["arguments"]; got != want {
			t.Fatal("output_item.done arguments differ from the streamed deltas")
		}
	})
}

func TestResponsesToCanonicalStream_AssemblesLongSummary(t *testing.T) {
	texts := accumulateDeltas(2000)
	chunks := [][]byte{
		responsesSSEChunk(ResponsesEventCreated, map[string]any{"response": map[string]any{"id": "resp_1", "model": "m", "status": "in_progress"}}),
		responsesSSEChunk(ResponsesEventOutputItemAdded, map[string]any{"output_index": 0, "item": map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}}}),
	}
	for _, d := range texts {
		chunks = append(chunks, responsesSSEChunk(ResponsesEventReasoningSummaryTextDelta, ResponsesReasoningSummaryTextDeltaEvent{ItemID: "rs_1", Delta: d}))
	}
	chunks = append(chunks, responsesSSEChunk(ResponsesEventOutputItemDone, map[string]any{"output_index": 0, "item": map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}}}))

	item := completedCanonicalItem(t, streamFrames(t, ResponsesTranslator{}.NewToCanonicalStream(), chunks))
	if got := item["summary"].([]any)[0].(map[string]any)["text"]; got != strings.Join(texts, "") {
		t.Fatal("reasoning summary differs from the streamed deltas")
	}
}
