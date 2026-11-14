package openai

import (
	"cmp"
	"encoding/json"
	"reflect"
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

// decodeMap decodes JSON bytes to a map for assertion without field coupling.
func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// jsonEqual reports whether a and b are valid JSON holding the same value.
func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// assertJSON fails unless got holds the JSON value want; key order and spacing do not matter.
func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	if !jsonEqual(got, []byte(want)) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// runStream feeds each frame, terminated with a blank line, through translate and returns the joined output.
func runStream(t *testing.T, translate func([]byte) ([]byte, error), frames ...string) []byte {
	t.Helper()
	var out []byte
	for i, f := range frames {
		b, err := translate([]byte(f + "\n\n"))
		if err != nil {
			t.Fatalf("frame %d %q: %v", i, f, err)
		}
		out = append(out, b...)
	}
	return out
}

// assertEvents checks the SSE frames in out against want, one "<event> <data>" entry per frame; a frame without an event line is named "data", and JSON data compares by value.
func assertEvents(t *testing.T, out []byte, want ...string) {
	t.Helper()
	frames := splitCanonicalFrames(out)
	for i := range max(len(frames), len(want)) {
		var got, w string
		if i < len(frames) {
			event, data, _ := v1.ParseSSEChunk(frames[i])
			got = cmp.Or(event, "data") + " " + string(data)
		}
		if i < len(want) {
			w = want[i]
		}
		gotEvent, gotData, _ := strings.Cut(got, " ")
		wantEvent, wantData, _ := strings.Cut(w, " ")
		if gotEvent != wantEvent || (gotData != wantData && !jsonEqual([]byte(gotData), []byte(wantData))) {
			t.Errorf("frame %d:\n got  %s\n want %s", i, got, w)
		}
	}
}

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
