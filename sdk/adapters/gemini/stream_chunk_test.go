package gemini

import (
	"bytes"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestToCanonicalStream_TwoEventsInOneChunk(t *testing.T) {
	first := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"m"}` + "\n\n")
	second := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}],"modelVersion":"m"}` + "\n\n")

	want, failed := feedStream(t, GeminiTranslator{}.NewToCanonicalStream(), [][]byte{first, second})
	if len(failed) > 0 {
		t.Fatalf("chunks %v failed", failed)
	}
	got, err := GeminiTranslator{}.NewToCanonicalStream()(append(append([]byte(nil), first...), second...))
	if err != nil {
		t.Fatal(err)
	}
	// The response id carries the stream's start second; compare with it masked.
	if !bytes.Equal(maskGeminiID(got), maskGeminiID(want)) {
		t.Errorf("joined chunk:\n%s\nwant:\n%s", got, want)
	}
	if names, want := eventNames(got), []string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemDelta}; !reflect.DeepEqual(names, want) {
		t.Errorf("events = %v, want %v", names, want)
	}
}

func maskGeminiID(b []byte) []byte {
	i := bytes.Index(b, []byte(`"gemini-`))
	if i < 0 {
		return b
	}
	j := bytes.IndexByte(b[i+1:], '"')
	return append(append(append([]byte(nil), b[:i]...), `"gemini-X`...), b[i+1+j:]...)
}

func TestExtractTokens_MultiLineData(t *testing.T) {
	body := []byte("data: {\"candidates\":[],\ndata: \"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5}}\n\n")
	got := ExtractTokens(body)
	if got["input"] != 3 || got["output"] != 5 {
		t.Errorf("tokens = %v, want input 3 output 5", got)
	}
}
