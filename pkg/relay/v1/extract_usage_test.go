package v1

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

// fakeTranslator only implements what ExtractUsage needs.
type fakeTranslator struct{}

func (fakeTranslator) ParseRequest(_ []byte) (*Request, error)             { return nil, nil }
func (fakeTranslator) SerializeRequest(_ *Request) ([]byte, error)         { return nil, nil }
func (fakeTranslator) SerializeResponse(_ *Response, _ *Request) ([]byte, error) {
	return nil, nil
}
func (fakeTranslator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (fakeTranslator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

func (fakeTranslator) ParseResponse(body []byte) (*Response, error) {
	var r struct {
		Usage usage.Tokens `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &Response{Usage: r.Usage}, nil
}

func TestExtractUsage_SyncJSON(t *testing.T) {
	body := []byte(`{"usage":{"input":12,"output":34}}`)
	u, err := ExtractUsage(fakeTranslator{}, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u["input"] != 12 || u["output"] != 34 {
		t.Fatalf("got %+v", u)
	}
}

func TestExtractUsage_Gzipped(t *testing.T) {
	plain := []byte(`{"usage":{"input":99,"output":42}}`)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(plain)
	_ = gw.Close()

	u, err := ExtractUsage(fakeTranslator{}, buf.Bytes())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u["input"] != 99 || u["output"] != 42 {
		t.Fatalf("got %+v", u)
	}
}

func TestExtractUsage_CanonicalSSE(t *testing.T) {
	completed, _ := json.Marshal(GenerationCompletedEvent{
		ID:     "resp_1",
		Status: "completed",
		Usage:  usage.Tokens{"input": 100, "output": 50, "cache_read": 25},
	})
	frame := SSEFrame{Event: EventGenerationCompleted, Data: completed}

	body := append(SSEFrame{Event: EventGenerationCreated, Data: []byte(`{}`)}.Bytes(),
		frame.Bytes()...)

	u, err := ExtractUsage(fakeTranslator{}, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u["input"] != 100 || u["output"] != 50 || u["cache_read"] != 25 {
		t.Fatalf("got %+v", u)
	}
}

func TestExtractUsage_EmptyBody(t *testing.T) {
	u, err := ExtractUsage(fakeTranslator{}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil, got %+v", u)
	}
}

func TestExtractUsage_NilTranslator(t *testing.T) {
	u, err := ExtractUsage(nil, []byte(`{"usage":{"input":1}}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil, got %+v", u)
	}
}

func TestLooksLikeSSE_Basic(t *testing.T) {
	cases := map[string]bool{
		"event: foo\ndata: {}": true,
		"data: {}":             true,
		`{"foo":1}`:            false,
	}
	for body, want := range cases {
		if got := looksLikeSSE([]byte(body)); got != want {
			t.Errorf("looksLikeSSE(%q): got %v want %v", body, got, want)
		}
	}
}
