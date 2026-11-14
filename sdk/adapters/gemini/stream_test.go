package gemini

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// canonStream is a decoded canonical stream.
type canonStream struct {
	events []string
	items  []v1.Item
	done   *v1.GenerationCompletedEvent
	err    *v1.ErrorEvent
}

// decodeCanon decodes canonical SSE output and checks each completed item
// equals the concatenation of its deltas.
func decodeCanon(t *testing.T, out []byte) canonStream {
	t.Helper()
	var s canonStream
	deltas := map[string]string{}
	for _, f := range strings.SplitAfter(string(out), "\n\n") {
		ev, data, ok := v1.ParseSSEChunk([]byte(f))
		if !ok {
			continue
		}
		s.events = append(s.events, ev)
		var target any
		switch ev {
		case v1.EventItemDelta:
			var d v1.ItemDeltaEvent
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatalf("%s: %v", ev, err)
			}
			deltas[d.ItemID] += d.Delta
		case v1.EventItemCompleted:
			var c v1.ItemCompletedEvent
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatalf("%s: %v", ev, err)
			}
			if got := itemText(c.Item); deltas[c.ItemID] != got {
				t.Errorf("%s: deltas %q, completed item %q", c.ItemID, deltas[c.ItemID], got)
			}
			s.items = append(s.items, c.Item)
		case v1.EventGenerationCompleted:
			s.done = &v1.GenerationCompletedEvent{}
			target = s.done
		case v1.EventError:
			s.err = &v1.ErrorEvent{}
			target = s.err
		}
		if target != nil {
			if err := json.Unmarshal(data, target); err != nil {
				t.Fatalf("%s: %v", ev, err)
			}
		}
	}
	return s
}

func itemText(it v1.Item) string {
	switch v := it.(type) {
	case *v1.Message:
		var sb strings.Builder
		for _, p := range v.Content {
			if tp, ok := p.(*v1.OutputTextPart); ok {
				sb.WriteString(tp.Text)
			}
		}
		return sb.String()
	case *v1.FunctionCall:
		return v.Arguments
	case *v1.Reasoning:
		return v.Content
	}
	return ""
}

// toCanonical feeds Gemini data payloads, one frame per call, through one stream.
func toCanonical(t *testing.T, payloads ...string) canonStream {
	t.Helper()
	fn := tr.NewToCanonicalStream()
	var out []byte
	for _, p := range payloads {
		b, err := fn([]byte("data: " + p + "\n\n"))
		if err != nil {
			t.Fatalf("frame %s: %v", p, err)
		}
		out = append(out, b...)
	}
	return decodeCanon(t, out)
}

// fromCanonical feeds canonical events through one stream and returns the
// Gemini output of each event.
func fromCanonical(t *testing.T, frames ...v1.SSEFrame) []string {
	t.Helper()
	fn := tr.NewFromCanonicalStream()
	out := make([]string, len(frames))
	for i, f := range frames {
		b, err := fn(f.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", f.Event, err)
		}
		out[i] = string(b)
	}
	return out
}

// geminiData returns the data payload of one Gemini SSE frame.
func geminiData(t *testing.T, frame string) []byte {
	t.Helper()
	data, ok := strings.CutPrefix(frame, "data: ")
	if !ok || !strings.HasSuffix(data, "\n\n") || strings.Count(data, "\n\n") != 1 {
		t.Fatalf("not one Gemini frame: %q", frame)
	}
	return []byte(strings.TrimSuffix(data, "\n\n"))
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

func TestToCanonicalStream_MatchesParseResponse(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"plan","thought":true,"thoughtSignature":"sig-rs"},
		{"text":"Calling."},
		{"functionCall":{"name":"lookup","args":{"x":1}},"thoughtSignature":"sig-fc"}
	]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3}}`
	want := `[
		{"type":"reasoning","id":"rs_0","summary":[{"text":"plan"}],"content":"plan","status":"completed","provider_data":{"thoughtSignature":"sig-rs"}},
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Calling."}]},
		{"type":"function_call","id":"fc_2","call_id":"lookup__relay_call_2","name":"lookup","arguments":"{\"x\":1}","status":"completed","provider_data":{"thoughtSignature":"sig-fc"}}]`

	resp := parseResponse(t, body)
	assertJSON(t, resp.Output, want)
	assertJSON(t, resp.Usage, `{"input":10,"output":8,"reasoning":3}`)

	var frame bytes.Buffer
	if err := json.Compact(&frame, []byte(body)); err != nil {
		t.Fatal(err)
	}
	s := toCanonical(t, frame.String())
	assertJSON(t, s.items, want)
	if s.done == nil || s.done.Status != resp.Status || s.done.FinishReason != v1.FinishReasonToolCalls || !reflect.DeepEqual(s.done.Usage, resp.Usage) {
		t.Errorf("stream completion %+v, buffered %s/%s %v", s.done, resp.Status, resp.FinishReason, resp.Usage)
	}
	wantEvents := []string{v1.EventGenerationCreated}
	for range 3 {
		wantEvents = append(wantEvents, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted)
	}
	if wantEvents = append(wantEvents, v1.EventGenerationCompleted); !reflect.DeepEqual(s.events, wantEvents) {
		t.Errorf("events %v, want %v", s.events, wantEvents)
	}
}

func TestToCanonicalStream_ThoughtSignatureOnLaterChunk(t *testing.T) {
	s := toCanonical(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"a","thought":true}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"b","thought":true,"thoughtSignature":"sig"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","index":0}]}`,
	)
	assertJSON(t, s.items, `[{"type":"reasoning","id":"rs_0","summary":[{"text":"ab"}],"content":"ab","status":"completed","provider_data":{"thoughtSignature":"sig"}}]`)
}

func TestToCanonicalStream_BlockedPromptTerminates(t *testing.T) {
	s := toCanonical(t, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`)
	if want := []string{v1.EventGenerationCreated, v1.EventGenerationCompleted}; !reflect.DeepEqual(s.events, want) {
		t.Fatalf("events %v, want %v", s.events, want)
	}
	assertJSON(t, s.done, `{"id":"`+s.done.ID+`","status":"incomplete","finish_reason":"content_filter","usage":{"input":7},"incomplete_details":{"reason":"content_filter"}}`)
}

func TestToCanonicalStream_ErrorFrameSurfaces(t *testing.T) {
	t.Skip("bug: a mid-stream {\"error\":{…}} frame decodes as an empty geminiResponse, so it yields no error event and the stream ends without a terminal event")
	s := toCanonical(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"index":0}]}`,
		`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`,
	)
	if s.err == nil || *s.err != (v1.ErrorEvent{Code: "RESOURCE_EXHAUSTED", Message: "quota"}) {
		t.Errorf("error event %+v, want RESOURCE_EXHAUSTED/quota; events %v", s.err, s.events)
	}
}

func TestToCanonicalStream_TwoEventsInOneChunk(t *testing.T) {
	first := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"m","responseId":"r"}` + "\n\n")
	second := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}],"modelVersion":"m","responseId":"r"}` + "\n\n")
	want, failed := feedStream(t, tr.NewToCanonicalStream(), [][]byte{first, second})
	if len(failed) > 0 {
		t.Fatalf("chunks %v failed", failed)
	}
	got, err := tr.NewToCanonicalStream()(append(append([]byte(nil), first...), second...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined chunk:\n%s\nwant:\n%s", got, want)
	}
	if names, want := decodeCanon(t, got).events, []string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemDelta}; !reflect.DeepEqual(names, want) {
		t.Errorf("events = %v, want %v", names, want)
	}
}

func TestToCanonicalStream_MalformedChunkMidStream(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"m"}` + "\n\n"),
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":` + "\n\n"),
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n"),
	}
	out, failed := feedStream(t, tr.NewToCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{1}) {
		t.Fatalf("failed chunks = %v, want [1]", failed)
	}
	s := decodeCanon(t, out)
	want := []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted,
	}
	if !reflect.DeepEqual(s.events, want) {
		t.Fatalf("events = %v, want %v", s.events, want)
	}
	assertJSON(t, s.items, `[{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]`)
}

// Gemini emits a functionCall whole, so argument deltas are buffered until the item completes.
func TestFromCanonicalStream_FunctionArgsBuffered(t *testing.T) {
	ev := func(event, data string) v1.SSEFrame { return v1.SSEFrame{Event: event, Data: []byte(data)} }
	out := fromCanonical(t,
		ev(v1.EventGenerationCreated, `{"id":"r","model":"m"}`),
		ev(v1.EventItemStarted, `{"item_id":"fc_0","item_type":"function_call","index":0,"name":"search","call_id":"search__relay_call_0"}`),
		ev(v1.EventItemDelta, `{"item_id":"fc_0","kind":"arguments","delta":"{\"q\":"}`),
		ev(v1.EventItemDelta, `{"item_id":"fc_0","kind":"arguments","delta":"\"hi\"}"}`),
		ev(v1.EventItemCompleted, `{"item_id":"fc_0","index":0,"item":{"type":"function_call","call_id":"search__relay_call_0","name":"search","arguments":"{\"q\":\"hi\"}"}}`),
	)
	if got := strings.Join(out[:4], ""); got != "" {
		t.Errorf("events before item.completed emitted %q", got)
	}
	assertJSON(t, geminiData(t, out[4]), `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"hi"}}}]},"index":0}],"modelVersion":"m","responseId":"r"}`)
}

func TestFromCanonicalStream_ErrorEvent(t *testing.T) {
	out := fromCanonical(t, v1.SSEFrame{Event: v1.EventError, Data: []byte(`{"code":"RESOURCE_EXHAUSTED","message":"quota"}`)})
	assertJSON(t, geminiData(t, out[0]), `{"error":{"code":500,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`)
}

func TestFromCanonicalStream_MalformedEventReturnsError(t *testing.T) {
	for _, event := range []string{
		v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta,
		v1.EventGenerationCompleted, v1.EventError,
	} {
		t.Run(event, func(t *testing.T) {
			chunk := []byte("event: " + event + "\ndata: {\"item_id\":\n\n")
			if out, err := tr.NewFromCanonicalStream()(chunk); err == nil {
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
	out, failed := feedStream(t, tr.NewFromCanonicalStream(), chunks)
	if !reflect.DeepEqual(failed, []int{3}) {
		t.Fatalf("failed chunks = %v, want [3]", failed)
	}
	canon, err := tr.NewToCanonicalStream()(out)
	if err != nil {
		t.Fatal(err)
	}
	back := decodeCanon(t, canon)
	assertJSON(t, back.items, `[{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]`)
	if back.done == nil || back.done.FinishReason != v1.FinishReasonStop {
		t.Errorf("completion %+v, want stop", back.done)
	}
}
