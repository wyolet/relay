package adapters_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var chunkingCases = map[string]string{
	"openai-chat":      "openai/chat-text",
	"openai-responses": "openai/responses-text",
	"anthropic":        "anthropic/text",
	"gemini":           "gemini/text",
}

// TestToCanonicalStreamTwoFramesInOneChunk replays a corpus stream with each
// adjacent pair of frames joined into one chunk in turn, and expects the same
// canonical output as feeding every frame on its own.
func TestToCanonicalStreamTwoFramesInOneChunk(t *testing.T) {
	for name, c := range chunkingCases {
		t.Run(name, func(t *testing.T) {
			s := shapeByName(name)
			frames := corpusFrames(t, c)
			want, err := translateFrames(s.tr.NewToCanonicalStream(), frames)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+1 < len(frames); i++ {
				joined := append(append([][]byte{}, frames[:i]...), bytes.Join(frames[i:i+2], nil))
				joined = append(joined, frames[i+2:]...)
				got, err := translateFrames(s.tr.NewToCanonicalStream(), joined)
				if err != nil {
					t.Fatalf("frames %d+%d: %v", i, i+1, err)
				}
				w, g := mustPrettyValue(t, recordFrames(want)), mustPrettyValue(t, recordFrames(got))
				if !bytes.Equal(w, g) {
					t.Fatalf("frames %d+%d translated differently from one at a time:\n%s", i, i+1, firstDifference(w, g))
				}
			}
		})
	}
}

// TestToCanonicalStreamMultiLineData replays a corpus stream with every JSON
// payload indented and written as one data: line per line, and expects the
// same canonical output as the single-line stream.
func TestToCanonicalStreamMultiLineData(t *testing.T) {
	for name, c := range chunkingCases {
		t.Run(name, func(t *testing.T) {
			s := shapeByName(name)
			frames := corpusFrames(t, c)
			want, err := translateFrames(s.tr.NewToCanonicalStream(), frames)
			if err != nil {
				t.Fatal(err)
			}
			split := make([][]byte, len(frames))
			multiLine := 0
			for i, f := range frames {
				split[i] = splitDataLines(f)
				if !bytes.Equal(split[i], f) {
					multiLine++
				}
			}
			if multiLine == 0 {
				t.Fatal("no frame was rewritten to multi-line data")
			}
			got, err := translateFrames(s.tr.NewToCanonicalStream(), split)
			if err != nil {
				t.Fatal(err)
			}
			w, g := mustPrettyValue(t, recordFrames(want)), mustPrettyValue(t, recordFrames(got))
			if !bytes.Equal(w, g) {
				t.Errorf("multi-line data translated differently from single-line:\n%s", firstDifference(w, g))
			}
		})
	}
}

func corpusFrames(t *testing.T, caseDir string) [][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", caseDir, streamInput))
	if err != nil {
		t.Fatal(err)
	}
	return splitFrames(body)
}

// splitDataLines rewrites a frame's JSON data line as indented JSON, one
// data: line per output line; other lines pass through.
func splitDataLines(frame []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimRight(frame, "\n"), []byte("\n")) {
		payload, ok := bytes.CutPrefix(line, []byte("data: "))
		var indented bytes.Buffer
		if !ok || json.Indent(&indented, payload, "", "  ") != nil {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		for _, l := range bytes.Split(indented.Bytes(), []byte("\n")) {
			fmt.Fprintf(&out, "data: %s\n", l)
		}
	}
	out.WriteByte('\n')
	return out.Bytes()
}
