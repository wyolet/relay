package gemini

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// feedStream runs chunks through one stream closure, returning the
// concatenated output and the indexes of chunks that returned an error.
func feedStream(t *testing.T, translate func([]byte) ([]byte, error), chunks [][]byte) ([]byte, []int) {
	t.Helper()
	var out []byte
	var failed []int
	for i, c := range chunks {
		b, err := translate(c)
		if err != nil {
			if len(b) != 0 {
				t.Errorf("chunk %d: output %q returned alongside error", i, b)
			}
			failed = append(failed, i)
			continue
		}
		out = append(out, b...)
	}
	return out, failed
}

func eventNames(b []byte) []string {
	var names []string
	for _, f := range strings.Split(string(b), "\n\n") {
		if ev, _, ok := v1.ParseSSEChunk([]byte(f)); ok {
			names = append(names, ev)
		}
	}
	return names
}

func TestToCanonicalStream_MalformedChunkMidStream(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"m"}` + "\n\n"),
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":` + "\n\n"),
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n"),
	}
	out, failed := feedStream(t, GeminiTranslator{}.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{1}) {
		t.Fatalf("failed chunks = %v, want [1]", failed)
	}
	want := []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted,
	}
	if got := eventNames(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestFromCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventGenerationCompleted, v1.EventError,
	} {
		t.Run(event, func(t *testing.T) {
			translate := GeminiTranslator{}.NewFromCanonicalStream()
			chunk := []byte("event: " + event + "\ndata: {\"item_id\":\n\n")
			if out, err := translate(chunk); err == nil {
				t.Fatalf("malformed canonical %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func TestFromCanonicalStream_MalformedEventMidStream(t *testing.T) {
	ev := func(event, data string) []byte { return []byte("event: " + event + "\ndata: " + data + "\n\n") }
	chunks := [][]byte{
		ev(v1.EventGenerationCreated, `{"id":"g1","model":"m"}`),
		ev(v1.EventItemStarted, `{"item_id":"msg_1","item_type":"message","index":0}`),
		ev(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"Hel"}`),
		ev(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":7}`),
		ev(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"lo"}`),
		ev(v1.EventGenerationCompleted, `{"id":"g1","status":"completed","finish_reason":"stop"}`),
	}
	out, failed := feedStream(t, GeminiTranslator{}.NewFromCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	for _, want := range []string{`"text":"Hel"`, `"text":"lo"`, `"finishReason":"STOP"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}
