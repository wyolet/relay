package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "github.com/wyolet/relay/sdk/v1"
)

const testModel = "claude-test"

var anchor = &v1.ItemCacheConfig{Anchor: true}

func ptr[T any](v T) *T { return &v }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func userText(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func systemText(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func assistantText(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: text}}}
}

func anchored(m *v1.Message) *v1.Message {
	m.CacheConfig = anchor
	return m
}

// assertJSONEqual compares two JSON documents by value, so member order and spacing do not matter.
func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	if diff := cmp.Diff(w, g); diff != "" {
		t.Errorf("JSON differs (-want +got):\n%s\ngot %s", diff, got)
	}
}

type sseEvent struct {
	Event string
	Data  any
}

// frames builds one SSE frame per (event, data) pair.
func frames(pairs ...string) [][]byte {
	var out [][]byte
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, []byte("event: "+pairs[i]+"\ndata: "+pairs[i+1]+"\n\n"))
	}
	return out
}

// decodeSSE reads a stream of single-line-data frames into events with decoded payloads.
func decodeSSE(t *testing.T, stream []byte) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, frame := range bytes.Split(bytes.TrimSuffix(stream, []byte("\n\n")), []byte("\n\n")) {
		if len(frame) == 0 {
			continue
		}
		event, data, ok := bytes.Cut(frame, []byte("\ndata: "))
		if !ok || !bytes.HasPrefix(event, []byte("event: ")) {
			t.Fatalf("frame is not event + data: %q", frame)
		}
		e := sseEvent{Event: string(event[len("event: "):])}
		if err := json.Unmarshal(data, &e.Data); err != nil {
			t.Fatalf("frame data is not JSON: %v\n%s", err, frame)
		}
		out = append(out, e)
	}
	return out
}

// translateAll feeds chunks through one stream translator and returns the concatenated output.
func translateAll(t *testing.T, translate func([]byte) ([]byte, error), chunks [][]byte) []byte {
	t.Helper()
	var out []byte
	for i, c := range chunks {
		b, err := translate(c)
		if err != nil {
			t.Fatalf("chunk %d %q: %v", i, c, err)
		}
		out = append(out, b...)
	}
	return out
}

func assertEvents(t *testing.T, got []byte, want ...string) {
	t.Helper()
	g, w := decodeSSE(t, got), decodeSSE(t, bytes.Join(frames(want...), nil))
	if diff := cmp.Diff(w, g); diff != "" {
		t.Errorf("events differ (-want +got):\n%s", diff)
	}
}
