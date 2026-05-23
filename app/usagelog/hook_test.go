package usagelog

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

// stubExtractor unmarshals a single JSON document and pulls "usage" fields.
// Mirrors the shape of real per-adapter extractors so we can verify the
// SSE walker accumulates correctly without depending on a vendor package.
type stubExtractor struct{}

func (stubExtractor) ExtractTokens(body []byte) usage.Tokens {
	var v struct {
		Usage *struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				Input int64 `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	t := usage.Tokens{}
	if v.Usage != nil {
		if v.Usage.Input > 0 {
			t["input"] = v.Usage.Input
		}
		if v.Usage.Output > 0 {
			t["output"] = v.Usage.Output
		}
	}
	if v.Message != nil && v.Message.Usage != nil && v.Message.Usage.Input > 0 {
		t["input"] = v.Message.Usage.Input
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

func TestExtractTokensFromBody_SyncJSON(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":12,"output_tokens":34}}`)
	got := extractTokensFromBody(stubExtractor{}, body)
	if got["input"] != 12 || got["output"] != 34 {
		t.Fatalf("sync: got %+v", got)
	}
}

func TestExtractTokensFromBody_SSEAccumulates(t *testing.T) {
	body := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1247}}}

event: content_block_delta
data: {"type":"content_block_delta"}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":418}}

event: message_stop
data: {"type":"message_stop"}

`)
	got := extractTokensFromBody(stubExtractor{}, body)
	if got["input"] != 1247 {
		t.Fatalf("input: got %d want 1247 (got map %+v)", got["input"], got)
	}
	if got["output"] != 418 {
		t.Fatalf("output: got %d want 418 (got map %+v)", got["output"], got)
	}
}

func TestLooksLikeSSE(t *testing.T) {
	cases := map[string]bool{
		`event: foo
data: {}`: true,
		`data: {}`:                                  true,
		`{"usage":{"input_tokens":12}}`:             false,
		`   {"foo":1}`:                              false,
		`   event: bar`:                             true,
		"":                                          false,
	}
	for body, want := range cases {
		if got := looksLikeSSE([]byte(body)); got != want {
			t.Errorf("looksLikeSSE(%q): got %v want %v", body, got, want)
		}
	}
}
