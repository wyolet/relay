package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- test helpers ----

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func floatPtr(v float64) *float64 { return &v }

func intPtr(v int) *int { return &v }

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

func sseChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// collectCanonEvents runs Anthropic SSE chunks through NewToCanonicalStream and returns event names.
func collectCanonEvents(t *testing.T, chunks [][]byte) []string {
	t.Helper()
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var names []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		for _, frame := range splitFrames(out) {
			ev, _, ok := v1.ParseSSEChunk(frame)
			if ok && ev != "" {
				names = append(names, ev)
			}
		}
	}
	return names
}

// splitFrames splits concatenated SSE frames on \n\n.
func splitFrames(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	var frames [][]byte
	parts := strings.Split(string(data), "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			frames = append(frames, []byte(p))
		}
	}
	return frames
}
