package v1

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

// feedFrames splits a full SSE body on the blank-line separator and feeds
// each frame (separator stripped, as a dispatch scanner would) to the
// summarizer — the same shape a live stream observer sees.
func feedFrames(s *StreamSummarizer, body []byte) {
	for _, frame := range bytes.Split(body, []byte("\n\n")) {
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		s.Observe(frame)
	}
}

// The incremental summarizer must land the SAME Summary as the batch
// ExtractSummary for the same stream — parity is the whole point.
func TestStreamSummarizer_ParityWithExtractSummary(t *testing.T) {
	completed, _ := json.Marshal(GenerationCompletedEvent{
		ID:           "resp_1",
		Status:       "completed",
		Usage:        usage.Tokens{"input": 100, "output": 50, "cache_read": 25},
		FinishReason: FinishReasonStop,
	})
	body := bytes.Join([][]byte{
		SSEFrame{Event: EventGenerationCreated, Data: []byte(`{}`)}.Bytes(),
		SSEFrame{Event: EventGenerationCompleted, Data: completed}.Bytes(),
	}, nil)

	batch, err := ExtractSummary(fakeTranslator{}, body)
	if err != nil {
		t.Fatalf("ExtractSummary: %v", err)
	}

	s := NewStreamSummarizer(fakeTranslator{})
	feedFrames(s, body)
	got := s.Summary()

	if !reflect.DeepEqual(got.Tokens, batch.Tokens) {
		t.Fatalf("tokens: incremental %+v != batch %+v", got.Tokens, batch.Tokens)
	}
	if got.FinishReason != batch.FinishReason {
		t.Fatalf("finish: incremental %q != batch %q", got.FinishReason, batch.FinishReason)
	}
	if got.Tokens["input"] != 100 || got.Tokens["output"] != 50 || got.Tokens["cache_read"] != 25 {
		t.Fatalf("tokens: %+v", got.Tokens)
	}
}

// A stream that never carries a usage block leaves the Summary zero — the
// same nothing ExtractSummary returns.
func TestStreamSummarizer_NoUsage(t *testing.T) {
	body := SSEFrame{Event: EventGenerationCreated, Data: []byte(`{}`)}.Bytes()
	s := NewStreamSummarizer(fakeTranslator{})
	feedFrames(s, body)
	if got := s.Summary(); len(got.Tokens) != 0 || got.FinishReason != "" {
		t.Fatalf("want zero summary, got %+v", got)
	}
}

// A nil translator is a no-op summarizer, matching ExtractSummary(nil, …).
func TestStreamSummarizer_NilTranslator(t *testing.T) {
	s := NewStreamSummarizer(nil)
	s.Observe([]byte("event: x\ndata: {}"))
	if got := s.Summary(); len(got.Tokens) != 0 || got.FinishReason != "" {
		t.Fatalf("nil translator must summarize nothing, got %+v", got)
	}
}
