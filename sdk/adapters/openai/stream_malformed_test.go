package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func sseEvent(event, data string) []byte {
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

func TestResponsesToCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range []string{
		ResponsesEventCreated,
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
	for _, event := range []string{ResponsesEventOutputItemAdded, ResponsesEventOutputItemDone} {
		var typeErr *json.UnmarshalTypeError
		if out, err := (ResponsesTranslator{}).NewToCanonicalStream()(sseEvent(event, `{"item":5}`)); !errors.As(err, &typeErr) {
			t.Errorf("%s with a mistyped item: err = %v, want the decode error (output %q)", event, err, out)
		}
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

// A malformed chunk fails alone: the stream around it carries on with its state intact.
func TestCCToCanonicalStream_MalformedChunkMidStream(t *testing.T) {
	chunks := [][]byte{
		[]byte(ccChunk(`{"role":"assistant","content":"Hel"}`) + "\n\n"),
		[]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\n\n"),
		[]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	out, failed := feedStream(t, CCTranslator{}.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{1}) {
		t.Fatalf("failed chunks = %v, want [1]", failed)
	}
	assertEvents(t, out,
		`generation.created {"id":"c1","model":"m"}`,
		`item.started {"item_id":"msg_0","item_type":"message","index":0}`,
		`item.delta {"item_id":"msg_0","index":0,"kind":"text","delta":"Hel"}`,
		`item.delta {"item_id":"msg_0","index":0,"kind":"text","delta":"lo"}`,
		`item.completed {"item_id":"msg_0","index":0,"item":{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
		`generation.completed {"id":"c1","status":"completed","finish_reason":"stop"}`,
	)
}

func TestFromCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, tr := range []v1.Translator{CCTranslator{}, ResponsesTranslator{}} {
		for _, event := range []string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted, v1.EventError} {
			if out, err := tr.NewFromCanonicalStream()(sseEvent(event, `{"item_id":`)); err == nil {
				t.Errorf("%T: malformed canonical %s: want error, got output %q", tr, event, out)
			}
		}
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
	const head = `"id":"g1","object":"chat.completion.chunk","model":"m"`
	assertCCChunks(t, out,
		`{`+head+`,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
}

func TestResponsesFromCanonicalStream_MalformedEventMidStream(t *testing.T) {
	bad := sseEvent(v1.EventItemDelta, `{"item_id":"msg_1","index":0,"kind":"text","delta":7}`)
	out, failed := feedStream(t, ResponsesTranslator{}.NewFromCanonicalStream(), canonicalTextStream(bad))
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	want := []string{
		ResponsesEventCreated, ResponsesEventInProgress, ResponsesEventOutputItemAdded, ResponsesEventContentPartAdded,
		ResponsesEventOutputTextDelta, ResponsesEventOutputTextDelta, ResponsesEventOutputTextDone,
		ResponsesEventContentPartDone, ResponsesEventOutputItemDone, ResponsesEventCompleted,
	}
	if got := extractCanonicalEvents(out); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
	if !bytes.Contains(out, []byte(`"text":"Hello"`)) {
		t.Errorf("the closing frames lost the text around the bad delta:\n%s", out)
	}
}
