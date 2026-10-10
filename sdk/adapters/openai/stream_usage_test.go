package openai

import (
	"encoding/json"
	"testing"
)

func TestRequestStreamUsage(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		changed   bool
		wantOpts  string
		wantOther string // another stream_options key that must survive
	}{
		{name: "streamed without options", body: `{"stream":true}`, changed: true, wantOpts: `{"include_usage":true}`},
		{name: "streamed with null options", body: `{"stream":true,"stream_options":null}`, changed: true, wantOpts: `{"include_usage":true}`},
		{name: "streamed with usage off", body: `{"stream":true,"stream_options":{"include_usage":false,"x":1}}`, changed: true, wantOther: "x"},
		{name: "already asks for usage", body: `{"stream":true,"stream_options":{"include_usage":true}}`},
		{name: "not streamed", body: `{"stream":false}`},
		{name: "no stream field", body: `{}`},
		{name: "malformed options", body: `{"stream":true,"stream_options":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.body), &fields); err != nil {
				t.Fatal(err)
			}
			if got := RequestStreamUsage(fields); got != tc.changed {
				t.Fatalf("changed = %v, want %v", got, tc.changed)
			}
			if !tc.changed {
				return
			}
			var opts map[string]json.RawMessage
			if err := json.Unmarshal(fields["stream_options"], &opts); err != nil {
				t.Fatalf("stream_options: %v", err)
			}
			if string(opts["include_usage"]) != "true" {
				t.Fatalf("include_usage = %s, want true", opts["include_usage"])
			}
			if tc.wantOpts != "" && string(fields["stream_options"]) != tc.wantOpts {
				t.Fatalf("stream_options = %s, want %s", fields["stream_options"], tc.wantOpts)
			}
			if tc.wantOther != "" {
				if _, ok := opts[tc.wantOther]; !ok {
					t.Fatalf("stream_options lost %q: %s", tc.wantOther, fields["stream_options"])
				}
			}
		})
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
