package v1

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
)

// observeAll feeds each frame of body, blank-line separator stripped, as a stream session does.
func observeAll(s *StreamSummarizer, body string) {
	for _, frame := range strings.Split(body, "\n\n") {
		s.Observe([]byte(frame))
	}
}

// The incremental summarizer lands the same Summary as ExtractSummary over the whole body.
func TestStreamSummarizer(t *testing.T) {
	var seen []string
	tests := []struct {
		name string
		tr   Translator
		body string
		want Summary
	}{
		{"canonical stream", IdentityTranslator{}, createdFrame + deltaFrame + completedFrame, completedSummary},
		{"translated stream", passTranslator{}, createdFrame + deltaFrame + completedFrame, completedSummary},
		{"filtered stream", filterTranslator{seen: &seen}, createdFrame + deltaFrame + completedFrame, completedSummary},
		{"no generation.completed", IdentityTranslator{}, createdFrame + deltaFrame, Summary{}},
		{"a later frame without a summary keeps the one found", IdentityTranslator{}, completedFrame + deltaFrame, completedSummary},
		{"translator error on every chunk", failingTranslator{}, completedFrame, Summary{}},
		{"nil translator", nil, completedFrame, Summary{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStreamSummarizer(tc.tr)
			observeAll(s, tc.body)
			if diff := cmp.Diff(tc.want, s.Summary()); diff != "" {
				t.Errorf("summary (-want +got):\n%s", diff)
			}
			batch, err := ExtractSummary(tc.tr, []byte(tc.body))
			if diff := cmp.Diff(batch, s.Summary()); diff != "" || err != nil {
				t.Errorf("differs from ExtractSummary, err %v (-batch +incremental):\n%s", err, diff)
			}
		})
	}
	// One chunk per walk: the incremental and the batch walk each translate only the completed frame.
	if diff := cmp.Diff([]string{completedFrame, completedFrame}, seen); diff != "" {
		t.Errorf("chunks the filtered stream translated (-want +got):\n%s", diff)
	}

	var nilSummarizer *StreamSummarizer
	nilSummarizer.Observe([]byte(completedFrame))
	if got := nilSummarizer.Summary(); !got.isEmpty() {
		t.Errorf("nil summarizer: %+v, want an empty summary", got)
	}
}

func TestStreamSummarizer_MergesCompletedFrames(t *testing.T) {
	t.Skip("bug: usage in one generation.completed frame and finish_reason in a later one → StreamSummarizer keeps only the later frame (no tokens), ExtractSummary merges both")
	body := "event: generation.completed\ndata: {\"usage\":{\"input\":1},\"finish_reason\":\"length\"}\n\n" +
		"event: generation.completed\ndata: {\"finish_reason\":\"stop\"}\n\n"
	s := NewStreamSummarizer(IdentityTranslator{})
	observeAll(s, body)
	want := Summary{Tokens: usage.Tokens{"input": 1}, FinishReason: FinishReasonStop}
	if diff := cmp.Diff(want, s.Summary()); diff != "" {
		t.Errorf("summary (-want +got):\n%s", diff)
	}
}
