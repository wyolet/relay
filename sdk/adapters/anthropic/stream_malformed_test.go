package anthropic

import (
	"bytes"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func rawSSE(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

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
	for _, f := range splitFrames(b) {
		if ev, _, ok := v1.ParseSSEChunk(f); ok {
			names = append(names, ev)
		}
	}
	return names
}

func TestToCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "message_delta"} {
		t.Run(event, func(t *testing.T) {
			translate := AnthropicTranslator{}.NewToCanonicalStream()
			// content_block_delta is only decoded inside an open block.
			if _, err := translate(contentBlockStartText(0)); err != nil {
				t.Fatal(err)
			}
			if out, err := translate(rawSSE(event, `{"delta":`)); err == nil {
				t.Fatalf("malformed %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func TestToCanonicalStream_MalformedEventMidStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_1", "m"),
		contentBlockStartText(0),
		textDeltaChunk(0, "Hel"),
		rawSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":7}}`),
		textDeltaChunk(0, "lo"),
		contentBlockStopChunk(0),
		rawSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`),
		rawSSE("message_stop", `{}`),
	}
	out, failed := feedStream(t, AnthropicTranslator{}.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
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
		v1.EventItemCompleted, v1.EventGenerationCompleted, v1.EventError,
	} {
		t.Run(event, func(t *testing.T) {
			translate := AnthropicTranslator{}.NewFromCanonicalStream()
			if out, err := translate(rawSSE(event, `{"item_id":`)); err == nil {
				t.Fatalf("malformed canonical %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func TestFromCanonicalStream_MalformedEventMidStream(t *testing.T) {
	chunks := [][]byte{
		rawSSE(v1.EventGenerationCreated, `{"id":"g1","model":"m"}`),
		rawSSE(v1.EventItemStarted, `{"item_id":"msg_1","item_type":"message","index":0}`),
		rawSSE(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"Hel"}`),
		rawSSE(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":7}`),
		rawSSE(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"lo"}`),
		rawSSE(v1.EventItemCompleted, `{"item_id":"msg_1","index":0}`),
		rawSSE(v1.EventGenerationCompleted, `{"id":"g1","status":"completed","finish_reason":"stop"}`),
	}
	out, failed := feedStream(t, AnthropicTranslator{}.NewFromCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	for _, want := range []string{`"text":"Hel"`, `"text":"lo"`, "event: message_stop"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}
