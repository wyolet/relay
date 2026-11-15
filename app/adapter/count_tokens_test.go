package adapter_test

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/adapters"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

func countingSpec() *adapter.Spec {
	return (&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		CountPath:   "/v1/messages/count_tokens",
		Auth:        adapter.AuthStrategy{Header: "x-api-key", ExtraHeaders: map[string]string{"x-api-version": "v1"}},
		Translator:  stubTranslator{},
		// A generation's extractor must not be consulted for a count reply.
		ExtractTokens:  func([]byte) pkgusage.Tokens { return pkgusage.Tokens{"input": 50} },
		UpstreamPathFn: func(string, bool) string { return "/generation-only" },
	}).Build()
}

// Only a spec with a CountPath offers the capability, so a caller's type assertion decides between counting and estimating.
func TestPipelineAdapter_TokenCounterNeedsCountPath(t *testing.T) {
	if _, ok := newBearerSpec("/v1/chat/completions").PipelineAdapter().(adapter.TokenCounter); ok {
		t.Error("a spec without CountPath satisfies TokenCounter")
	}
	if _, ok := countingSpec().PipelineAdapter().(adapter.TokenCounter); !ok {
		t.Error("a spec with CountPath does not satisfy TokenCounter")
	}
}

// The host path and the per-model path builder name the generation endpoint; a count ignores both.
func TestCountAdapter_Call(t *testing.T) {
	baseURL, last := captureServer(t)
	count := countingSpec().PipelineAdapter().(adapter.TokenCounter).CountAdapter()

	resp, err := count.Call(t.Context(), baseURL, ptr("/host-override"), "sk-test", []byte(`{"model":"m"}`), http.Header{"X-Beta": {"b1"}}, "m", true, false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	want := captured{
		Method: http.MethodPost,
		URI:    "/v1/messages/count_tokens",
		Header: http.Header{
			"Accept-Encoding": {"gzip"},
			"Content-Length":  {"13"},
			"Content-Type":    {"application/json"},
			"User-Agent":      {"Go-http-client/1.1"},
			"X-Api-Key":       {"sk-test"},
			"X-Api-Version":   {"v1"},
			"X-Beta":          {"b1"},
		},
		Body: []byte(`{"model":"m"}`),
	}
	if diff := cmp.Diff(want, last()); diff != "" {
		t.Errorf("count request (-want +got):\n%s", diff)
	}

	if resp, err := count.Call(t.Context(), "http://bad host", nil, "k", nil, nil, "", false, false); err == nil || resp != nil {
		t.Errorf("unparseable base URL: resp = %v, err = %v", resp, err)
	}
	if resp, err := count.Call(t.Context(), "http://127.0.0.1:1", nil, "k", nil, nil, "", false, false); err == nil || resp != nil {
		t.Errorf("nothing listening: resp = %v, err = %v", resp, err)
	}
}

// A count consumes no tokens: its adapter reports none and reads no Summary, so post-flight charges nothing.
func TestCountAdapter_ChargesNothing(t *testing.T) {
	count := countingSpec().PipelineAdapter().(adapter.TokenCounter).CountAdapter()
	if got := count.ExtractTokens([]byte(`{"input_tokens":50}`)); got != nil {
		t.Errorf("tokens = %v, want nil", got)
	}
	if sum, ok := count.(pipeline.ResponseSummarizer).SummarizeResponse([]byte(`{"input_tokens":50}`)); ok {
		t.Errorf("the count adapter summarized its reply: %+v", sum)
	}
}

func TestParseTokenCount(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    int
		wantErr string
	}{
		{"count", 200, `{"input_tokens":1234}`, 1234, ""},
		{"upstream error carries its body", 400, " {\"error\":{\"message\":\"bad model\"}}\n", 0,
			`adapter: count_tokens upstream returned 400: {"error":{"message":"bad model"}}`},
		{"a non-200 success is not a count", 201, `{"input_tokens":7}`, 0, `adapter: count_tokens upstream returned 201: {"input_tokens":7}`},
		{"no input_tokens", 200, `{"something_else":1}`, 0, "adapter: count_tokens response carried no input_tokens"},
		{"zero", 200, `{"input_tokens":0}`, 0, "adapter: count_tokens response carried no input_tokens"},
		{"negative", 200, `{"input_tokens":-3}`, 0, "adapter: count_tokens response carried no input_tokens"},
		{"not JSON", 200, `<html>`, 0, "adapter: count_tokens response: invalid character '<' looking for beginning of value"},
		{"empty body", 200, ``, 0, "adapter: count_tokens response: unexpected end of JSON input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := adapter.ParseTokenCount(tt.status, []byte(tt.body))
			var got string
			if err != nil {
				got = err.Error()
			}
			if n != tt.want || got != tt.wantErr {
				t.Errorf("ParseTokenCount = (%d, %q), want (%d, %q)", n, got, tt.want, tt.wantErr)
			}
		})
	}
}
