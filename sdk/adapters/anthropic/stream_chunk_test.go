package anthropic

import (
	"bytes"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestToCanonicalStream_TwoEventsInOneChunk(t *testing.T) {
	first := sseChunk("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": "m", "usage": map[string]any{"input_tokens": 3}}})
	second := sseChunk("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})

	separate := AnthropicTranslator{}.NewToCanonicalStream()
	var want []byte
	for _, c := range [][]byte{first, second} {
		b, err := separate(c)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, b...)
	}
	got, err := AnthropicTranslator{}.NewToCanonicalStream()(append(append([]byte(nil), first...), second...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined chunk:\n%s\nwant:\n%s", got, want)
	}
	var names []string
	for _, f := range splitFrames(got) {
		if ev, _, ok := v1.ParseSSEChunk(f); ok {
			names = append(names, ev)
		}
	}
	if want := []string{v1.EventGenerationCreated, v1.EventItemStarted}; !reflect.DeepEqual(names, want) {
		t.Errorf("events = %v, want %v", names, want)
	}
}

func TestExtractTokens_MultiLineData(t *testing.T) {
	body := []byte("event: message_start\ndata: {\"type\":\"message_start\",\ndata: \"message\":{\"usage\":{\"input_tokens\":3,\"cache_creation_input_tokens\":7}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
	got := ExtractTokens(body)
	if got["input"] != 3 || got["cache_creation"] != 7 || got["output"] != 5 {
		t.Errorf("tokens = %v, want input 3 cache_creation 7 output 5", got)
	}
}
