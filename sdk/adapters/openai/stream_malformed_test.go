package openai

import (
	"bytes"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func sseEvent(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

func ccDataFrame(data string) []byte {
	return []byte("data: " + data + "\n\n")
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

func TestResponsesToCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range []string{
		ResponsesEventOutputItemAdded,
		ResponsesEventOutputTextDelta,
		ResponsesEventFunctionCallArgumentsDelta,
		ResponsesEventReasoningTextDelta,
		ResponsesEventReasoningSummaryTextDelta,
		ResponsesEventRefusalDelta,
		ResponsesEventOutputItemDone,
		ResponsesEventCompleted,
		ResponsesEventIncomplete,
		ResponsesEventError,
	} {
		t.Run(event, func(t *testing.T) {
			translate := ResponsesTranslator{}.NewToCanonicalStream()
			if out, err := translate(sseEvent(event, `{"item":`)); err == nil {
				t.Fatalf("malformed %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func TestResponsesToCanonicalStream_TerminalWithoutResponseReturnsError(t *testing.T) {
	translate := ResponsesTranslator{}.NewToCanonicalStream()
	if _, err := translate(sseEvent(ResponsesEventCompleted, `{"type":"response.completed"}`)); err == nil {
		t.Fatal("response.completed without a response object: want error, got nil")
	}
}

func TestResponsesToCanonicalStream_MalformedFailedEventSurfacesError(t *testing.T) {
	translate := ResponsesTranslator{}.NewToCanonicalStream()
	out, err := translate(sseEvent(ResponsesEventFailed, `{"response":`))
	if err != nil {
		t.Fatalf("response.failed is terminal and must surface as a canonical event, got error %v", err)
	}
	if got := extractCanonicalEvents(out); !reflect.DeepEqual(got, []string{v1.EventError}) {
		t.Fatalf("events = %v, want [error]", got)
	}
}

func TestResponsesToCanonicalStream_MalformedEventMidStream(t *testing.T) {
	chunks := [][]byte{
		sseEvent(ResponsesEventCreated, `{"response":{"id":"resp_1","model":"m","created_at":1}}`),
		sseEvent(ResponsesEventOutputItemAdded, `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
		sseEvent(ResponsesEventOutputTextDelta, `{"item_id":"msg_1","output_index":0,"delta":"Hel"}`),
		sseEvent(ResponsesEventOutputTextDelta, `{"item_id":"msg_1","output_index":0,"delta":42}`),
		sseEvent(ResponsesEventOutputTextDelta, `{"item_id":"msg_1","output_index":0,"delta":"lo"}`),
		sseEvent(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}}`),
		sseEvent(ResponsesEventCompleted, `{"response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`),
	}
	out, failed := feedStream(t, ResponsesTranslator{}.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	want := []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted,
	}
	if got := extractCanonicalEvents(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestCCToCanonicalStream_MalformedChunkMidStream(t *testing.T) {
	chunks := [][]byte{
		ccDataFrame(`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`),
		ccDataFrame(`{"id":"c1","choices":[{"index":0,"delta":{"content":`),
		ccDataFrame(`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`),
		ccDataFrame(`[DONE]`),
	}
	out, failed := feedStream(t, CCTranslator{}.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{1}) {
		t.Fatalf("failed chunks = %v, want [1]", failed)
	}
	want := []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted,
	}
	if got := extractCanonicalEvents(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

var canonicalStreamEvents = []string{
	v1.EventGenerationCreated,
	v1.EventItemStarted,
	v1.EventItemDelta,
	v1.EventItemCompleted,
	v1.EventGenerationCompleted,
	v1.EventError,
}

func TestCCFromCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range canonicalStreamEvents {
		t.Run(event, func(t *testing.T) {
			translate := CCTranslator{}.NewFromCanonicalStream()
			if out, err := translate(sseEvent(event, `{"item_id":`)); err == nil {
				t.Fatalf("malformed canonical %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func TestResponsesFromCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range canonicalStreamEvents {
		t.Run(event, func(t *testing.T) {
			translate := ResponsesTranslator{}.NewFromCanonicalStream()
			if out, err := translate(sseEvent(event, `{"item_id":`)); err == nil {
				t.Fatalf("malformed canonical %s: want error, got nil (output %q)", event, out)
			}
		})
	}
}

func canonicalTextStream(badDelta []byte) [][]byte {
	return [][]byte{
		sseEvent(v1.EventGenerationCreated, `{"id":"g1","model":"m"}`),
		sseEvent(v1.EventItemStarted, `{"item_id":"msg_1","item_type":"message","index":0}`),
		sseEvent(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"Hel"}`),
		badDelta,
		sseEvent(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":"lo"}`),
		sseEvent(v1.EventItemCompleted, `{"item_id":"msg_1","index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`),
		sseEvent(v1.EventGenerationCompleted, `{"id":"g1","status":"completed","finish_reason":"stop"}`),
	}
}

func TestCCFromCanonicalStream_MalformedEventMidStream(t *testing.T) {
	bad := sseEvent(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":7}`)
	out, failed := feedStream(t, CCTranslator{}.NewFromCanonicalStream(), canonicalTextStream(bad))
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	for _, want := range []string{`"content":"Hel"`, `"content":"lo"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}

func TestResponsesFromCanonicalStream_MalformedEventMidStream(t *testing.T) {
	bad := sseEvent(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":7}`)
	out, failed := feedStream(t, ResponsesTranslator{}.NewFromCanonicalStream(), canonicalTextStream(bad))
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	for _, want := range []string{`"delta":"Hel"`, `"delta":"lo"`, "event: " + ResponsesEventCompleted} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}
