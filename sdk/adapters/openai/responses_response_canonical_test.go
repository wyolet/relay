package openai

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// The Responses API has no finish_reason: it is derived from status, incomplete_details.reason and the output items, and a cut-off must never read as a clean stop.
func TestResponsesParseResponse_FinishReason(t *testing.T) {
	const (
		call    = `{"type":"function_call","id":"fc_0","call_id":"c0","name":"f","arguments":"{}"}`
		custom  = `{"type":"custom_tool_call","id":"ctc_0","call_id":"c0","name":"f","input":"x"}`
		refusal = `{"type":"message","id":"msg_0","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`
	)
	cases := []struct {
		name, status, details, output string
		want                          v1.FinishReason
	}{
		{"plain stop", "completed", "null", "", v1.FinishReasonStop},
		{"function call", "completed", "null", call, v1.FinishReasonToolCalls},
		{"custom tool call", "completed", "null", custom, v1.FinishReasonToolCalls},
		{"refusal part", "completed", "null", refusal, v1.FinishReasonRefusal},
		{"refusal next to a call", "completed", "null", refusal + "," + call, v1.FinishReasonToolCalls},
		{"truncated", "incomplete", `{"reason":"max_output_tokens"}`, "", v1.FinishReasonLength},
		{"content filter", "incomplete", `{"reason":"content_filter"}`, call, v1.FinishReasonContentFilter},
		{"incomplete without details", "incomplete", "null", "", v1.FinishReasonOther},
		{"failed", "failed", "null", "", ""},
		{"cancelled", "cancelled", "null", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := respParseResponse(t, `{"id":"r","object":"response","model":"m","status":"`+tc.status+`","incomplete_details":`+tc.details+`,"output":[`+tc.output+`]}`)
			if resp.Status != v1.Status(tc.status) || resp.FinishReason != tc.want {
				t.Errorf("status/finish_reason = %q/%q, want %q/%q", resp.Status, resp.FinishReason, tc.status, tc.want)
			}
		})
	}
}

func TestResponsesParseResponse(t *testing.T) {
	resp := respParseResponse(t, `{"id":"resp_1","object":"response","created_at":7,"status":"completed","model":"gpt-5","service_tier":"flex","output":[
		{"type":"web_search_call","id":"ws_0","status":"completed","action":{"type":"search","query":"go"}},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Checking"}],"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}]},
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[
			{"type":"output_text","text":"See [1].","annotations":[{"type":"url_citation","start_index":4,"end_index":7,"url":"https://x","title":"X"},{"type":"file_citation","file_id":"file_abc","index":3}]},
			{"type":"output_audio","data":"UklGRg==","transcript":"hi"}]}],
		"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":150},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":40},"total_tokens":300}}`)
	respJSONEq(t, resp, `{"id":"resp_1","object":"response","created_at":7,"model":"gpt-5","status":"completed","finish_reason":"stop","service_tier":"flex",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[{"text":"Checking"}],"content":"221 = 13 * 17"},
			{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"See [1].","annotations":[
				{"type":"url_citation","start_index":4,"end_index":7,"url":"https://x","title":"X"},
				{"type":"file_citation","file_id":"file_abc","index":3}]}]}],
		"usage":{"input":50,"output":100,"cache_read":150,"reasoning":40}}`)
}

// What ParseResponse keeps, SerializeResponse writes back in the same wire form.
func TestResponsesResponseRoundTrip(t *testing.T) {
	const output = `[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"BLOB","status":"completed"},
		{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"See [1].","annotations":[{"type":"file_citation","file_id":"file_abc","index":3}],"logprobs":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{}","status":"completed"}]`
	resp := respParseResponse(t, `{"id":"r","object":"response","created_at":7,"status":"completed","model":"m","output":`+output+`}`)
	wire, err := ResponsesTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	respJSONEq(t, respMember(t, wire, "output"), output)
}

// Responses carries length, content_filter and other only as status=incomplete plus incomplete_details.reason.
func TestResponsesSerializeResponse_Status(t *testing.T) {
	cases := []struct {
		name string
		resp v1.Response
		want string
	}{
		{"stop", v1.Response{Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}, `{"status":"completed","incomplete_details":null,"error":null}`},
		{"tool calls", v1.Response{FinishReason: v1.FinishReasonToolCalls}, `{"status":"completed","incomplete_details":null,"error":null}`},
		{"length", v1.Response{Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonLength}, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"error":null}`},
		{"length keeps the raw reason", v1.Response{FinishReason: v1.FinishReasonLength, IncompleteDetails: &v1.IncompleteDetails{Reason: "anthropic:max_tokens"}}, `{"status":"incomplete","incomplete_details":{"reason":"anthropic:max_tokens"},"error":null}`},
		{"content filter", v1.Response{Status: v1.StatusCompleted, FinishReason: v1.FinishReasonContentFilter}, `{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"error":null}`},
		{"content filter keeps the raw reason", v1.Response{FinishReason: v1.FinishReasonContentFilter, IncompleteDetails: &v1.IncompleteDetails{Reason: "gemini:SAFETY"}}, `{"status":"incomplete","incomplete_details":{"reason":"gemini:SAFETY"},"error":null}`},
		{"other", v1.Response{Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: &v1.IncompleteDetails{Reason: "anthropic:new_reason"}}, `{"status":"incomplete","incomplete_details":{"reason":"anthropic:new_reason"},"error":null}`},
		{"other without details", v1.Response{Status: v1.StatusCompleted, FinishReason: v1.FinishReasonOther}, `{"status":"incomplete","incomplete_details":null,"error":null}`},
		{"failed", v1.Response{Status: v1.StatusFailed, Error: &v1.Error{Code: "server_error", Message: "boom"}}, `{"status":"failed","incomplete_details":null,"error":{"code":"server_error","message":"boom"}}`},
		{"cancelled with details", v1.Response{Status: v1.Status("cancelled"), IncompleteDetails: &v1.IncompleteDetails{Reason: "user"}}, `{"status":"cancelled","incomplete_details":{"reason":"user"},"error":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.ID, tc.resp.Model = "r", "m"
			wire, err := ResponsesTranslator{}.SerializeResponse(&tc.resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if fr := respMember(t, wire, "finish_reason"); fr != nil {
				t.Errorf("finish_reason is not a Responses field, got %s", fr)
			}
			got := map[string]json.RawMessage{}
			for _, k := range []string{"status", "incomplete_details", "error"} {
				got[k] = respMember(t, wire, k)
			}
			respJSONEq(t, got, tc.want)
		})
	}
}

func TestResponsesSerializeResponse_EmptyOutput(t *testing.T) {
	wire, err := ResponsesTranslator{}.SerializeResponse(&v1.Response{ID: "r", Model: "m", Status: v1.StatusFailed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	respJSONEq(t, respMember(t, wire, "output"), `[]`)
}

// v1.RawAnnotation exists to carry annotation types canonical does not model.
func TestResponsesParseResponse_UnmodeledAnnotation(t *testing.T) {
	t.Skip(`bug: ParseResponse output_text annotation {"type":"container_file_citation",…} → dropped from canonical annotations without a trace`)
	resp := respParseResponse(t, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"container_file_citation","container_id":"c","file_id":"f"}]}]}]}`)
	respJSONEq(t, resp.Output, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"container_file_citation","container_id":"c","file_id":"f"}]}]}]`)
}

// Responses input_tokens includes cached tokens and output_tokens includes reasoning; canonical meters do not overlap.
func TestResponsesUsage(t *testing.T) {
	cases := []struct {
		name  string
		wire  ResponsesUsage
		canon usage.Tokens
	}{
		{"cached and reasoning", ResponsesUsage{InputTokens: 5504, OutputTokens: 104, TotalTokens: 5608, InputTokensDetails: ResponsesInputDeets{CachedTokens: 5120}, OutputTokensDetails: ResponsesOutputDeets{ReasoningTokens: 49}},
			usage.Tokens{"input": 384, "cache_read": 5120, "output": 104, "reasoning": 49}},
		{"all cached", ResponsesUsage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11, InputTokensDetails: ResponsesInputDeets{CachedTokens: 10}}, usage.Tokens{"cache_read": 10, "output": 1}},
		{"zero", ResponsesUsage{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respJSONEq(t, responsesUsageToCanonical(&tc.wire), respMarshal(t, tc.canon))
			if tc.canon != nil {
				respJSONEq(t, canonicalUsageToResponses(tc.canon), respMarshal(t, tc.wire))
			}
		})
	}
	// Chat Completions shares the rule: total is prompt + completion, never the sum of the meters.
	cc := canonicalUsageToCC(cases[0].canon)
	if cc.PromptTokens != 5504 || cc.CompletionTokens != 104 || cc.TotalTokens != 5608 {
		t.Errorf("CC usage = %d/%d/%d, want 5504/104/5608", cc.PromptTokens, cc.CompletionTokens, cc.TotalTokens)
	}
}

func respMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResponsesWireMarshal(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		// output is required on the wire even when the tool returned nothing.
		{"empty tool result", &ResponsesFunctionCallOutput{CallID: "c"}, `{"type":"function_call_output","call_id":"c","output":""}`},
		{"tool result with empty parts", &ResponsesFunctionCallOutput{CallID: "c", Output: "x", Content: []ResponsesPart{}}, `{"type":"function_call_output","call_id":"c","output":"x"}`},
		// summary is required on a reasoning item, which commonly has none.
		{"reasoning without summary", &ResponsesReasoning{ID: "rs_1", EncryptedContent: "blob"}, `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"blob"}`},
		{"tool choice auto", ResponsesToolChoice{Mode: "auto"}, `"auto"`},
		{"tool choice unset", ResponsesToolChoice{}, `"auto"`},
		{"tool choice function", ResponsesToolChoice{Mode: "function", FunctionName: "f"}, `{"type":"function","name":"f"}`},
		// Only auto/required/none are legal bare strings; a hosted-tool mode rebuilt from canonical has lost its object.
		{"tool choice hosted, rebuilt", ResponsesToolChoice{Mode: "file_search"}, `{"type":"file_search"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("got  %s\nwant %s", b, tc.want)
			}
		})
	}
}

// The type a part reports is the discriminator it writes on the wire.
func TestResponsesPartTypeMatchesWire(t *testing.T) {
	for _, p := range []ResponsesPart{&ResponsesTextPart{}, &ResponsesImagePart{}, &ResponsesFilePart{}, &ResponsesOutputTextPart{}, &ResponsesRefusalPart{}} {
		var wire struct{ Type ResponsesPartType }
		if err := json.Unmarshal([]byte(respMarshal(t, p)), &wire); err != nil || wire.Type != p.ResponsesPartType() {
			t.Errorf("%T reports %q, writes %q (%v)", p, p.ResponsesPartType(), wire.Type, err)
		}
	}
}

// Unmodeled items, parts, annotations and tool-choice objects re-encode byte for byte.
func TestResponsesWireVerbatimRoundTrip(t *testing.T) {
	decodeItem := func(b []byte) (any, error) { return responsesUnmarshalItem(b) }
	decodeChoice := func(b []byte) (any, error) {
		var c ResponsesToolChoice
		return c, json.Unmarshal(b, &c)
	}
	cases := []struct {
		name   string
		in     string
		decode func([]byte) (any, error)
	}{
		{"hosted-tool item", `{"type":"mcp_call","id":"mcp_0","name":"lookup","arguments":"{}","output":"ok"}`, decodeItem},
		{"unmodeled part and annotation", `{"type":"message","id":"m","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"container_file_citation","container_id":"c"}],"logprobs":[]},{"type":"output_audio","data":"UklGRg==","transcript":"hi"}]}`, decodeItem},
		{"required", `"required"`, decodeChoice},
		{"hosted tool choice", `{"type":"file_search"}`, decodeChoice},
		{"allowed_tools choice", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}`, decodeChoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.decode([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.in {
				t.Errorf("got  %s\nwant %s", out, tc.in)
			}
		})
	}
}
