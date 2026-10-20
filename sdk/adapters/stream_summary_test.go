package adapters_test

import (
	"bytes"
	"path/filepath"
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
	all := unfilteredTranslator{s.tr}
	variants := []struct {
		name string
		got  v1.Summary
	}{
		{"ExtractSummary CRLF", mustExtractSummary(t, s.tr, crlf)},
		{"StreamSummarizer", summarizeFrames(s.tr, body)},
		{"StreamSummarizer CRLF", summarizeFrames(s.tr, crlf)},
		{"unfiltered ExtractSummary", mustExtractSummary(t, all, body)},
		{"unfiltered StreamSummarizer", summarizeFrames(all, body)},
		{"unfiltered StreamSummarizer CRLF", summarizeFrames(all, crlf)},
	}
	for _, v := range variants {
		if got := mustPrettyValue(t, summaryGolden(v.got)); !bytes.Equal(want, got) {
			t.Errorf("%s differs from ExtractSummary:\n%s", v.name, firstDifference(want, got))
		}
	}
	checkFramesSkipped(t, s, dir, body)
}

// unfilteredTranslator hides a translator's v1.SummaryStream, so summarizers
// built on it translate every frame.
type unfilteredTranslator struct{ v1.Translator }

// everyFrameMatters lists the recorded streams in which no frame can be
// skipped, keyed by case dir.
var everyFrameMatters = map[string]string{
	"testdata/openai/chat-service-tier": "the only content chunk is the first to carry the tier; the rest carry finish, usage and [DONE]",
	"testdata/gemini/cache-usage":       "a single frame carrying the finish",
	"testdata/gemini/tool-calls":        "a functionCall frame, then the finish",
	"testdata/gemini/tool-call-ids":     "a functionCall frame, then the finish",
}

// checkFramesSkipped keeps the filtered-vs-unfiltered comparison above from
// passing vacuously: each recorded stream must have frames the shape's
// summary predicate rejects, unless everyFrameMatters says why not.
func checkFramesSkipped(t *testing.T, s shape, dir string, body []byte) {
	t.Helper()
	ss, ok := s.tr.(v1.SummaryStream)
	if !ok {
		t.Fatalf("%s does not implement v1.SummaryStream", s.name)
	}
	toCanon, affects := ss.NewSummaryStream()
	skipped := 0
	for _, frame := range splitFrames(body) {
		if !affects(bytes.TrimRight(frame, "\n")) {
			skipped++
			continue
		}
		// The predicate may read the state earlier frames left behind.
		_, _ = toCanon(frame)
	}
	why, exempt := everyFrameMatters[filepath.ToSlash(dir)]
	switch {
	case exempt && skipped > 0:
		t.Errorf("%s skipped %d frames; delete its everyFrameMatters entry (%s)", dir, skipped, why)
	case !exempt && skipped == 0:
		t.Errorf("%s: the summary predicate skipped no frame", dir)
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
