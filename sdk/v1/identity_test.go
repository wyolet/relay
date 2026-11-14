package v1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestIdentityTranslator_Request(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"cache_config":{"anchor":true}}],` +
		`"tools":{"definitions":[{"type":"function","name":"search","parameters":{}}]},"cache_config":{"instructions":true,"tools":true},"output_mode":"stream"}`
	tr := IdentityTranslator{}

	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, req); diff != "" {
		t.Errorf("ParseRequest differs from Parse (-want +got):\n%s", diff)
	}
	out, err := tr.SerializeRequest(req)
	if err != nil || string(out) != body {
		t.Errorf("SerializeRequest = %s, %v\nwant %s", out, err, body)
	}

	if _, err := tr.ParseRequest([]byte(`{"input":"hi"}`)); err == nil || err.Error() != "model required" {
		t.Errorf("ParseRequest without a model: %v, want Parse's error", err)
	}
}

func TestIdentityTranslator_Response(t *testing.T) {
	tr := IdentityTranslator{}
	resp, err := tr.ParseResponse([]byte(fullResponseWire))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(fullResponse(), resp); diff != "" {
		t.Errorf("ParseResponse (-want +got):\n%s", diff)
	}
	// The request is not consulted: a canonical response serializes the same for any caller.
	out, err := tr.SerializeResponse(resp, &Request{Model: ModelRefs{"other"}})
	if err != nil || string(out) != fullResponseWire {
		t.Errorf("SerializeResponse = %s, %v\nwant %s", out, err, fullResponseWire)
	}
}

func TestIdentityTranslator_Streams(t *testing.T) {
	tr := IdentityTranslator{}
	if tr.NewToCanonicalStream() != nil || tr.NewFromCanonicalStream() != nil {
		t.Error("stream factories must be nil: canonical chunks pass through untranslated")
	}
	if got, want := tr.RequestKeys(), (RequestKeys{Stream: "output_mode", StreamOn: "stream"}); got != want {
		t.Errorf("RequestKeys = %+v, want %+v", got, want)
	}
}
