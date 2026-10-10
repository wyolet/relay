package openai

import (
	"bytes"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestCCToCanonicalStream_TwoEventsInOneChunk(t *testing.T) {
	first := []byte(`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n")
	second := []byte(`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n")
	assertJoinedChunkMatches(t, CCTranslator{}.NewToCanonicalStream, first, second,
		[]string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemDelta})
}

func TestResponsesToCanonicalStream_TwoEventsInOneChunk(t *testing.T) {
	first := []byte("event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hel"}` + "\n\n")
	second := []byte("event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"lo"}` + "\n\n")
	assertJoinedChunkMatches(t, ResponsesTranslator{}.NewToCanonicalStream, first, second,
		[]string{v1.EventItemDelta, v1.EventItemDelta})
}

// assertJoinedChunkMatches feeds first+second as one chunk and expects the
// output of feeding them one at a time, with the given canonical events.
func assertJoinedChunkMatches(t *testing.T, newStream func() func([]byte) ([]byte, error), first, second []byte, events []string) {
	t.Helper()
	separate := newStream()
	var want []byte
	for _, c := range [][]byte{first, second} {
		b, err := separate(c)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, b...)
	}
	got, err := newStream()(append(append([]byte(nil), first...), second...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined chunk:\n%s\nwant:\n%s", got, want)
	}
	if names := extractCanonicalEvents(got); !reflect.DeepEqual(names, events) {
		t.Errorf("events = %v, want %v", names, events)
	}
}

func TestExtractTokens_MultiLineData(t *testing.T) {
	body := []byte("data: {\"choices\":[],\ndata: \"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	got := ExtractTokens(body)
	if got["input"] != 3 || got["output"] != 5 {
		t.Errorf("tokens = %v, want input 3 output 5", got)
	}
}
