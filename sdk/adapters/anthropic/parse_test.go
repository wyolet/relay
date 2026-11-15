package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"max_tokens":8,"metadata":{"user_id":"u"},"messages":[{"role":"user"}]}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	want := &MessagesRequest{Model: "m", Stream: true, MaxTokens: 8, Metadata: map[string]string{"user_id": "u"}, Messages: []json.RawMessage{json.RawMessage(`{"role":"user"}`)}, Raw: body}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	for _, metadata := range []string{`null`, `{}`, `{"user_id":5}`, `{"user_id":""}`} {
		got, err := Parse([]byte(`{"model":"m","max_tokens":8,"metadata":` + metadata + `}`))
		if err != nil || got.Metadata != nil {
			t.Errorf("metadata %s: parsed metadata %v, err %v, want none", metadata, got.Metadata, err)
		}
	}
}

func TestParse_Rejects(t *testing.T) {
	for body, message := range map[string]string{
		`{"model":`:                    "request body is not valid JSON",
		`{"max_tokens":8}`:             "model is required",
		`{"model":"m"}`:                "max_tokens is required",
		`{"model":"m","max_tokens":0}`: "max_tokens is required",
	} {
		_, err := Parse([]byte(body))
		status, envelope, ok := ParseError(err)
		if !ok || status != http.StatusBadRequest {
			t.Fatalf("%s: err %v, status %d, ok %v, want a 400 parse error", body, err, status, ok)
		}
		assertJSONEqual(t, envelope, `{"type":"error","error":{"type":"invalid_request_error","message":"`+message+`"}}`)
		if err.Error() != string(envelope) {
			t.Errorf("%s: Error() = %q, want the envelope %q", body, err.Error(), envelope)
		}
	}
	if status, envelope, ok := ParseError(errors.New("other")); ok || status != 0 || envelope != nil {
		t.Errorf("ParseError on a foreign error = %d, %q, %v", status, envelope, ok)
	}
}
