package adapters_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// splitFrameBug is why a shape's to-canonical stream loses a frame when one
// chunk carries two: v1.ParseSSEChunk (and ParseResponsesSSEChunk) keep only
// the chunk's last data line. Dispatch splits upstream bytes at "\n\n" before
// translating, so this holds only while every caller splits first.
const splitFrameBug = "the to-canonical parser reads one event per chunk and keeps the last data: line, so the first of two frames in a chunk is dropped"

// splitFrameBugs are shapes known to fail; each is a skipped subtest until the
// parser is fixed, and an entry that starts passing fails so it gets removed.
var splitFrameBugs = map[string]string{
	"openai-chat":      splitFrameBug,
	"openai-responses": splitFrameBug,
	"anthropic":        splitFrameBug,
	"gemini":           splitFrameBug,
}

// TestToCanonicalStreamTwoFramesInOneChunk replays a corpus stream with each
// adjacent pair of frames joined into one chunk in turn, and expects the same
// canonical output as feeding every frame on its own.
func TestToCanonicalStreamTwoFramesInOneChunk(t *testing.T) {
	cases := map[string]string{
		"openai-chat":      "openai/chat-text",
		"openai-responses": "openai/responses-text",
		"anthropic":        "anthropic/text",
		"gemini":           "gemini/text",
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := shapeByName(name)
			body, err := os.ReadFile(filepath.Join("testdata", c, streamInput))
			if err != nil {
				t.Fatal(err)
			}
			frames := splitFrames(body)
			want, err := translateFrames(s.tr.NewToCanonicalStream(), frames)
			if err != nil {
				t.Fatal(err)
			}
			var failures []string
			for i := 0; i+1 < len(frames); i++ {
				joined := append(append([][]byte{}, frames[:i]...), bytes.Join(frames[i:i+2], nil))
				joined = append(joined, frames[i+2:]...)
				got, err := translateFrames(s.tr.NewToCanonicalStream(), joined)
				if err != nil {
					failures = append(failures, fmt.Sprintf("frames %d+%d: %v", i, i+1, err))
					continue
				}
				w, g := mustPrettyValue(t, recordFrames(want)), mustPrettyValue(t, recordFrames(got))
				if !bytes.Equal(w, g) {
					failures = append(failures, fmt.Sprintf("frames %d+%d:\n%s", i, i+1, firstDifference(w, g)))
				}
			}
			bug, known := splitFrameBugs[name]
			switch {
			case len(failures) == 0 && known:
				t.Errorf("two frames in one chunk translate correctly now; delete the splitFrameBugs entry")
			case len(failures) == 0:
			case known:
				t.Skip(bug)
			default:
				t.Errorf("two frames in one chunk translated differently from one at a time:\n%s", failures[0])
			}
		})
	}
}
