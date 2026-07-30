package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// mustJSON encodes v to JSON, panicking on error. Used for test fixture construction.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func floatPtr(v float64) *float64 { return &v }

func intPtr(v int) *int { return &v }

func boolPtr(v bool) *bool { return &v }

// decodeMap decodes JSON bytes to a map for assertion without field coupling.
func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// --- ParseRequest ---

// extractCanonicalEvents parses concatenated canonical SSE bytes and collects event names.
func extractCanonicalEvents(b []byte) []string {
	var names []string
	for _, frame := range splitCanonicalFrames(b) {
		event, _, ok := v1.ParseSSEChunk(frame)
		if ok && event != "" {
			names = append(names, event)
		}
	}
	return names
}

// splitCanonicalFrames splits concatenated SSE bytes at \n\n.
func splitCanonicalFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		idx := indexDoubleNewline(b)
		if idx < 0 {
			if len(strings.TrimSpace(string(b))) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		frame := b[:idx+2]
		if len(strings.TrimSpace(string(b[:idx]))) > 0 {
			frames = append(frames, frame)
		}
		b = b[idx+2:]
	}
	return frames
}

func indexDoubleNewline(b []byte) int {
	for i := 0; i < len(b)-1; i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}

// --- Ollama reasoning divergence (reasoning vs reasoning_content) ---
