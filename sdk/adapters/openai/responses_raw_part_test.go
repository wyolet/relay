package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const audioPart = `{"type":"output_audio","data":"UklGRg==","transcript":"hi"}`

// An unmodeled part survives a same-shape decode and re-encode byte for byte.
func TestResponsesRawPartRoundTripsVerbatim(t *testing.T) {
	body := []byte(`{"id":"r","object":"response","status":"completed","output":[{"type":"message","id":"m","status":"completed","role":"assistant","content":[` + audioPart + `]}]}`)
	resp, err := UnmarshalResponsesResponse(body)
	if err != nil {
		t.Fatalf("UnmarshalResponsesResponse: %v", err)
	}
	msg := resp.Output[0].(*ResponsesMessage)
	raw, ok := msg.Content[0].(*ResponsesRawPart)
	if !ok || raw.Type != "output_audio" {
		t.Fatalf("part = %#v, want a ResponsesRawPart of type output_audio", msg.Content[0])
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(audioPart)) {
		t.Fatalf("re-encoded message lost the part verbatim: %s", out)
	}
}

// Request input is the caller's: an unmodeled part is refused cross-shape rather than dropped.
func TestResponsesParseRequestRefusesUnmodeledPart(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"listen"},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`)
	_, err := (ResponsesTranslator{}).ParseRequest(body)
	if err == nil || !strings.Contains(err.Error(), `"input_audio"`) {
		t.Fatalf("ParseRequest with an input_audio part: err = %v, want an unsupported part type error", err)
	}
	if _, err := ParseResponsesRequest(body); err != nil {
		t.Fatalf("the wire decode a same-shape pass relies on refused it: %v", err)
	}
}
