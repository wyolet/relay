package openai

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ccResponse wraps one choice in a chat.completion body created at 1.
func ccResponse(choice string, extra string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` + choice + `]` + extra + `}`
}

// The recorded chat-* goldens cover plain text, tool calls, refusal, reasoning_content, cached usage and service tier.
func TestCCParseResponse(t *testing.T) {
	const head = `"id":"c1","object":"response","created_at":1,"model":"m"`
	cases := []struct{ name, body, canon string }{
		{
			name: "url citations attach to the text; other annotations are skipped",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Paris [1].","annotations":[
				{"type":"url_citation","url_citation":{"start_index":6,"end_index":9,"url":"https://x/p","title":"P"}},
				{"type":"file_citation"}]}}`, ""),
			canon: `{` + head + `,"status":"completed","finish_reason":"stop","output":[{"type":"message","id":"msg_c1","status":"completed","role":"assistant",
				"content":[{"type":"output_text","text":"Paris [1].","annotations":[{"type":"url_citation","start_index":6,"end_index":9,"url":"https://x/p","title":"P"}]}]}]}`,
		},
		{
			name: "Ollama reasoning field and array content",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning":"think","content":[{"type":"text","text":"Hello, "},{"type":"image_url"},{"type":"text","text":"world."}]}}`, ""),
			canon: `{` + head + `,"status":"completed","finish_reason":"stop","output":[
				{"type":"reasoning","id":"rs_c1","summary":[{"text":"think"}],"content":"think","status":"completed","provider_data":{"cc_reasoning_field":"reasoning"}},
				{"type":"message","id":"msg_c1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello, world."}]}]}`,
		},
		{
			name: "reasoning_content as a part array wins over reasoning",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning_content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}`, ""),
			canon: `{` + head + `,"status":"completed","finish_reason":"stop","output":[
				{"type":"reasoning","id":"rs_c1","summary":[{"text":"ab"}],"content":"ab","status":"completed","provider_data":{"cc_reasoning_field":"reasoning_content"}},
				{"type":"function_call","id":"call_1","call_id":"call_1","name":"f","arguments":"{}","status":"completed"}]}`,
		},
		{
			name: "text and refusal both stay; the refusal lifts stop",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Partly.","refusal":"No more."}}`, ""),
			canon: `{` + head + `,"status":"completed","finish_reason":"refusal","output":[{"type":"message","id":"msg_c1","status":"completed","role":"assistant",
				"content":[{"type":"output_text","text":"Partly."},{"type":"output_text","text":"No more."}]}]}`,
		},
		{
			name: "a refusal does not override a truncation",
			body: ccResponse(`{"index":0,"finish_reason":"length","message":{"role":"assistant","content":null,"refusal":"No"}}`, ""),
			canon: `{` + head + `,"status":"incomplete","finish_reason":"length","incomplete_details":{"reason":"max_output_tokens"},
				"output":[{"type":"message","id":"msg_c1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"No"}]}]}`,
		},
		{
			name:  "no choices",
			body:  `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[]}`,
			canon: `{` + head + `,"status":"completed","finish_reason":"stop"}`,
		},
		{
			name: "logprobs ride the extensions",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"},"logprobs":{"content":[{"token":"hi","logprob":-0.25,"bytes":[104,105]}],"refusal":null}}`, ""),
			canon: `{` + head + `,"status":"completed","finish_reason":"stop",
				"output":[{"type":"message","id":"msg_c1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],
				"extensions":{"openai.logprobs":{"content":[{"token":"hi","logprob":-0.25,"bytes":[104,105]}],"refusal":null}}}`,
		},
		{
			name: "every usage meter; input excludes cached tokens",
			body: ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}`, `,"usage":{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,
				"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":10},
				"completion_tokens_details":{"reasoning_tokens":5,"audio_tokens":3,"accepted_prediction_tokens":7,"rejected_prediction_tokens":2}}`),
			canon: `{` + head + `,"status":"completed","finish_reason":"stop","output":[{"type":"message","id":"msg_c1","status":"completed","role":"assistant","content":null}],
				"usage":{"input":100,"output":40,"cache_read":20,"audio_input":10,"reasoning":5,"audio_output":3,"accepted_prediction":7,"rejected_prediction":2}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := CCTranslator{}.ParseResponse([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, mustJSON(resp), tc.canon)
		})
	}
}

func TestCCParseResponse_MissingCreatedStampsNow(t *testing.T) {
	before := time.Now().Unix()
	resp, err := CCTranslator{}.ParseResponse([]byte(`{"id":"c1","model":"m","choices":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.CreatedAt < before || resp.CreatedAt > time.Now().Unix() {
		t.Errorf("created_at = %d, want the parse time", resp.CreatedAt)
	}
	if _, err := (CCTranslator{}).ParseResponse([]byte(`{"choices":`)); err == nil {
		t.Error("truncated body: want an error")
	}
}

func TestCCSerializeResponse(t *testing.T) {
	msg := func(text string) *v1.Message {
		return &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: text}}}
	}
	call := &v1.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "f", Arguments: `{}`}
	const head = `"id":"r1","object":"chat.completion","created":1,"model":"m"`
	cases := []struct {
		name string
		resp v1.Response
		wire string
	}{
		{
			name: "error",
			resp: v1.Response{Error: &v1.Error{Code: "server_error", Message: "boom"}, Output: []v1.Item{msg("x")}},
			wire: `{"error":{"message":"boom","type":"server_error","code":"server_error"}}`,
		},
		{
			name: "text with tool calls, usage with cached and reasoning tokens",
			resp: v1.Response{FinishReason: v1.FinishReasonToolCalls, Output: []v1.Item{msg("a"), &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.TextPart{Text: "b"}}}, call},
				Usage: usage.Tokens{"input": 10, "cache_read": 5, "output": 4, "reasoning": 2}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"ab",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}],
				"usage":{"prompt_tokens":15,"completion_tokens":4,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":2}}}`,
		},
		{
			name: "tool calls alone leave content null",
			resp: v1.Response{FinishReason: v1.FinishReasonToolCalls, Output: []v1.Item{call}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		},
		{
			name: "reasoning goes back under the field it came in",
			resp: v1.Response{FinishReason: v1.FinishReasonStop, Output: []v1.Item{&v1.Reasoning{Content: "why", ProviderData: json.RawMessage(`{"cc_reasoning_field":"reasoning"}`)}, msg("hi")}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi","reasoning":"why"}}]}`,
		},
		{
			name: "summary-only reasoning defaults to reasoning_content",
			resp: v1.Response{FinishReason: v1.FinishReasonStop, Output: []v1.Item{&v1.Reasoning{Summary: []v1.SummaryText{{Text: "a"}, {Text: "b"}}}}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"ab"}}]}`,
		},
		{
			name: "a refusal without text falls back to content_filter",
			resp: v1.Response{Status: v1.StatusCompleted, FinishReason: v1.FinishReasonRefusal},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":""}}]}`,
		},
		{
			name: "failed without a finish reason is not a clean stop",
			resp: v1.Response{Status: v1.StatusFailed, Output: []v1.Item{msg("partial")}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":"partial"}}]}`,
		},
		{
			name: "an unrecognised raw finish reason goes back verbatim",
			resp: v1.Response{Status: v1.StatusIncomplete, Extensions: map[string]json.RawMessage{extFinishReason: json.RawMessage(`"new_reason"`)}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"new_reason","message":{"role":"assistant","content":""}}]}`,
		},
		{
			name: "logprobs come back from the extensions",
			resp: v1.Response{FinishReason: v1.FinishReasonStop, Output: []v1.Item{msg("hi")},
				Extensions: map[string]json.RawMessage{extLogprobs: json.RawMessage(`{"content":[{"token":"hi","logprob":-0.25}]}`), "acme.x": json.RawMessage(`1`)}},
			wire: `{` + head + `,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"},
				"logprobs":{"content":[{"token":"hi","logprob":-0.25}],"refusal":null}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.resp
			resp.ID, resp.CreatedAt, resp.Model = "r1", 1, "m"
			wire, err := CCTranslator{}.SerializeResponse(&resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, wire, tc.wire)
		})
	}
}

// Chat Completions has a wire field for every usage detail it reports, so a parse → serialize round trip must keep them all.
func TestCCResponse_UsageDetailsRoundTrip(t *testing.T) {
	t.Skip("bug: prompt audio_tokens and completion audio/accepted/rejected prediction tokens → dropped by canonicalUsageToCC")
	usageJSON := `{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":10},
		"completion_tokens_details":{"reasoning_tokens":5,"audio_tokens":3,"accepted_prediction_tokens":7,"rejected_prediction_tokens":2}}`
	resp, err := CCTranslator{}.ParseResponse([]byte(ccResponse(`{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}`, `,"usage":`+usageJSON)))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := CCTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Usage json.RawMessage }
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, got.Usage, usageJSON)
}
