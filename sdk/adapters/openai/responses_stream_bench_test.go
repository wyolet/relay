package openai

import (
	"strconv"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// benchCanonicalStream is a canonical stream of one item of itemType whose
// deltas of kind are delta(0..n-1).
func benchCanonicalStream(itemType v1.ItemType, kind v1.DeltaKind, n int, delta func(i int) string) [][]byte {
	frames := [][]byte{
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_bench", Model: "gpt-5"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "it_0", ItemType: itemType, Index: 0, Name: "write", CallID: "call_1"}),
	}
	for i := range n {
		frames = append(frames, canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "it_0", Index: 0, Kind: kind, Delta: delta(i)}))
	}
	return append(frames,
		canonicalChunk(v1.EventItemCompleted, map[string]any{"item_id": "it_0", "index": 0, "item": map[string]any{"status": "completed"}}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "resp_bench", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}),
	)
}

func BenchmarkResponsesFromCanonicalStream(b *testing.B) {
	b.Run("text_150", func(b *testing.B) {
		benchResponsesFromCanonical(b, benchCanonicalStream(v1.ItemTypeMessage, v1.DeltaKindText, 150, func(i int) string {
			return " tok" + strconv.Itoa(i)
		}))
	})
	b.Run("text_4k", func(b *testing.B) {
		benchResponsesFromCanonical(b, benchCanonicalStream(v1.ItemTypeMessage, v1.DeltaKindText, benchLongDeltas, benchLongDeltaText))
	})
	b.Run("tool_args_4k", func(b *testing.B) {
		benchResponsesFromCanonical(b, benchCanonicalStream(v1.ItemTypeFunctionCall, v1.DeltaKindArguments, benchLongDeltas, benchLongDeltaText))
	})
}

func benchResponsesFromCanonical(b *testing.B, frames [][]byte) {
	b.ReportAllocs()
	for b.Loop() {
		tc := ResponsesTranslator{}.NewFromCanonicalStream()
		for _, f := range frames {
			if _, err := tc(f); err != nil {
				b.Fatal(err)
			}
		}
	}
}
