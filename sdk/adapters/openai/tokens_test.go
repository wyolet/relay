package openai

import (
	"maps"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

func TestExtractTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want usage.Tokens
	}{
		{
			name: "every meter; input excludes cached tokens",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,
				"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":30},
				"completion_tokens_details":{"reasoning_tokens":10,"audio_tokens":12,"accepted_prediction_tokens":40,"rejected_prediction_tokens":8}}}`,
			want: usage.Tokens{"input": 80, "output": 50, "cache_read": 20, "audio_input": 30, "reasoning": 10, "audio_output": 12, "accepted_prediction": 40, "rejected_prediction": 8},
		},
		{
			name: "zero details stay out",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},
				"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}`,
			want: usage.Tokens{"input": 10, "output": 5},
		},
		{name: "prompt only", body: `{"usage":{"prompt_tokens":5}}`, want: usage.Tokens{"input": 5}},
		{name: "all cached", body: `{"usage":{"prompt_tokens":9,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":9}}}`, want: usage.Tokens{"output": 1, "cache_read": 9}},
		{name: "zero usage", body: `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`},
		{name: "no usage", body: `{"id":"c","choices":[{"delta":{"content":"hello"}}]}`},
		{name: "malformed", body: `{bad json`},
		{name: "blank", body: " \n\t"},
		{
			name: "stream: the last usage frame wins; non-JSON frames are skipped",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":12},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
				"data: {\"choices\":[],\"usage\":null}\n\n" +
				"data: [DONE]\n\n",
			want: usage.Tokens{"input": 30, "output": 7, "cache_read": 12, "reasoning": 3},
		},
		{
			name: "stream: one event split over data lines",
			body: "data: {\"choices\":[],\ndata: \"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n",
			want: usage.Tokens{"input": 3, "output": 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractTokens([]byte(tc.body))
			if !maps.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Errorf("tokens = %v, want %v", got, tc.want)
			}
		})
	}
}
