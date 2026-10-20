package adapters_test

import (
	"bytes"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const streamSummaryGolden = "stream.summary.json"

// summaryGolden is the golden form of a v1.Summary.
type summaryGolden struct {
	Tokens       usage.Tokens    `json:"tokens"`
	FinishReason v1.FinishReason `json:"finish_reason"`
	ServiceTier  string          `json:"service_tier"`
}

// runStreamSummary pins the Summary ExtractSummary harvests from the buffered
// stream; the incremental StreamSummarizer must land on the same Summary, with
// LF and with CRLF line endings.
func runStreamSummary(t *testing.T, s shape, dir string, body []byte, known map[string]bool) {
	t.Helper()
	sum, err := v1.ExtractSummary(s.tr, body)
	if err != nil {
		golden(t, dir, streamSummaryGolden, known, nil, err)
		return
	}
	want := mustPrettyValue(t, summaryGolden(sum))
	golden(t, dir, streamSummaryGolden, known, want, nil)

	crlf := bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
	variants := []struct {
		name string
		got  v1.Summary
	}{
		{"ExtractSummary CRLF", mustExtractSummary(t, s.tr, crlf)},
		{"StreamSummarizer", summarizeFrames(s.tr, body)},
		{"StreamSummarizer CRLF", summarizeFrames(s.tr, crlf)},
	}
	for _, v := range variants {
		if got := mustPrettyValue(t, summaryGolden(v.got)); !bytes.Equal(want, got) {
			t.Errorf("%s differs from ExtractSummary:\n%s", v.name, firstDifference(want, got))
		}
	}
}

func mustExtractSummary(t *testing.T, tr v1.Translator, body []byte) v1.Summary {
	t.Helper()
	sum, err := v1.ExtractSummary(tr, body)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// summarizeFrames feeds body to a StreamSummarizer the way the server's stream
// session reframes a tee'd body: complete frames as they split off, the
// trailing partial frame at end of stream.
func summarizeFrames(tr v1.Translator, body []byte) v1.Summary {
	s := v1.NewStreamSummarizer(tr)
	rest := body
	for {
		n, frame, _ := v1.SplitSSEFrames(rest, false)
		if n == 0 {
			break
		}
		if len(bytes.TrimSpace(frame)) > 0 {
			s.Observe(v1.NormalizeSSELineEnds(frame))
		}
		rest = rest[n:]
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		s.Observe(v1.NormalizeSSELineEnds(rest))
	}
	return s.Summary()
}
