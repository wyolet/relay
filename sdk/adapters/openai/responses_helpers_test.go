package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// responsesSSEChunk frames one Responses event the way an upstream sends it.
func responsesSSEChunk(event string, data any) []byte {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

// canonicalChunk frames one canonical event the way dispatch hands it to NewFromCanonicalStream.
func canonicalChunk(event string, data any) []byte { return responsesSSEChunk(event, data) }

// respSSE frames a Responses event whose data is already JSON text.
func respSSE(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

// respFrame is one SSE frame a stream translator emitted.
type respFrame struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// respStream feeds chunks one at a time through a stream function and returns every frame it emitted.
func respStream(t *testing.T, fn func([]byte) ([]byte, error), chunks ...[]byte) []respFrame {
	t.Helper()
	var frames []respFrame
	for i, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		for _, f := range splitSSEFrames(out) {
			event, data, ok := v1.ParseSSEChunk(f)
			if !ok {
				t.Fatalf("chunk %d emitted a frame without data: %q", i, f)
			}
			frames = append(frames, respFrame{Event: event, Data: append(json.RawMessage(nil), data...)})
		}
	}
	return frames
}

func respEvents(frames []respFrame) []string {
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.Event
	}
	return names
}

// respData returns the data of the only frame named event.
func respData(t *testing.T, frames []respFrame, event string) json.RawMessage {
	t.Helper()
	var found []json.RawMessage
	for _, f := range frames {
		if f.Event == event {
			found = append(found, f.Data)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s frames in %v, want 1", len(found), event, respEvents(frames))
	}
	return found[0]
}

// respJSONEq fails unless got (JSON bytes, or a value to marshal) encodes the same JSON value as want.
func respJSONEq(t *testing.T, got any, want string) {
	t.Helper()
	var gb []byte
	switch g := got.(type) {
	case []byte:
		gb = g
	case json.RawMessage:
		gb = g
	default:
		var err error
		if gb, err = json.Marshal(got); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	var g, w any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatalf("got is not JSON: %v: %s", err, gb)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v: %s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		var gc, wc bytes.Buffer
		_ = json.Compact(&gc, gb)
		_ = json.Compact(&wc, []byte(want))
		t.Errorf("JSON differs\n got: %s\nwant: %s", gc.Bytes(), wc.Bytes())
	}
}

// respMember returns one top-level member of a JSON object, nil when absent.
func respMember(t *testing.T, body []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, body)
	}
	return m[key]
}

func respParseRequest(t *testing.T, body string) *v1.Request {
	t.Helper()
	req, err := ResponsesTranslator{}.ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	return req
}

func respSerializeRequest(t *testing.T, req *v1.Request) []byte {
	t.Helper()
	b, err := ResponsesTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatalf("SerializeRequest: %v", err)
	}
	return b
}

func respParseResponse(t *testing.T, body string) *v1.Response {
	t.Helper()
	resp, err := ResponsesTranslator{}.ParseResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	return resp
}

func respPtr[T any](v T) *T { return &v }

func respUserText(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: text}}}
}
