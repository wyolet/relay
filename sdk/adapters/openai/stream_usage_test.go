package openai

import (
	"encoding/json"
	"testing"
)

func TestRequestStreamUsage(t *testing.T) {
	cases := []struct {
		body string
		want string // the stream_options value to set; empty leaves the request alone
	}{
		{`{"stream":true}`, `{"include_usage":true}`},
		{`{"stream":true,"stream_options":null}`, `{"include_usage":true}`},
		{`{"stream":true,"stream_options":{"include_usage":false,"x":1}}`, `{"include_usage":true,"x":1}`},
		{`{"stream":true,"stream_options":{"include_usage":"true"}}`, `{"include_usage":true}`},
		{`{"stream":true,"stream_options":{"include_usage":true}}`, ""},
		{`{"stream":false}`, ""},
		{`{"stream":"true"}`, ""},
		{`{}`, ""},
		{`{"stream":true,"stream_options":"yes"}`, ""},
	}
	for _, tc := range cases {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.body), &fields); err != nil {
			t.Fatal(err)
		}
		key, value, changed := RequestStreamUsage(func(k string) []byte { return fields[k] })
		if changed != (tc.want != "") || (changed && (key != "stream_options" || string(value) != tc.want)) {
			t.Errorf("%s: got %q=%s (changed %v), want stream_options=%s", tc.body, key, value, changed, tc.want)
		}
	}
}

func TestIsUsageOnlyChunk(t *testing.T) {
	cases := []struct {
		frame string
		want  bool
	}{
		{`data: {"id":"c","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`, true},
		{`data: {"id": "c", "choices": [ ], "usage": {"prompt_tokens": 4}}`, true},
		{`data: {"id":"c","choices":[{"delta":{"content":"hi"}}],"usage":null}`, false},
		{`data: {"id":"c","choices":[],"usage":null}`, false},
		{`data: {"id":"c","choices":[]}`, false},
		{`data: [DONE]`, false},
		{`data: {"usage":{"prompt_tokens":4}}`, false},
		{`data: {"choices":[],`, false},
		{`data: {"choices":[{"delta":{}}],"x":{"choices":[]},"usage":{"prompt_tokens":4}}`, false},
		{`data: {"a":["choices",[]],"usage":{"prompt_tokens":4}}`, false},
		{`data: {"x":{"choices":"]"},"usage":{"prompt_tokens":4}}`, false},
		{`data: {"x":{"choices":[1]},"usage":{"prompt_tokens":4}}`, false},
		{`data: {"x":{"choices":[{}]},"usage":{"prompt_tokens":4}}`, false},
		{`data: "choices"`, false},
		{`data: {"choices":`, false},
		{`data: {"choices":[`, false},
		{`data: {"choices":[{"delta":{"content":"\"choices\":[]"}}],"usage":null}`, false},
		{"data: {\"id\":\"c\",\"choices\":\ndata: [],\"usage\":{\"prompt_tokens\":4}}", true},
		{"data: {\"id\":\"c\",\"choices\":[],\r\ndata: \"usage\":{\"prompt_tokens\":4}}", true},
		{"data: {\"id\":\"c\",\"choices\":[],\rdata: \"usage\":{\"prompt_tokens\":4}}", true},
		{": keepalive\ndata: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":4}}", true},
		{": {\"choices\":[],\"usage\":{\"prompt_tokens\":4}}", false},
		{"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4}}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}", true},
		{"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}", false},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4}}", false},
	}
	for _, tc := range cases {
		if got := IsUsageOnlyChunk([]byte(tc.frame)); got != tc.want {
			t.Errorf("IsUsageOnlyChunk(%s) = %v, want %v", tc.frame, got, tc.want)
		}
	}
}
