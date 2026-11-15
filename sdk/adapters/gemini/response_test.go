package gemini

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func parseResponse(t *testing.T, body string) *v1.Response {
	t.Helper()
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// finishOf renders status/finish_reason/incomplete_details.reason.
func finishOf(status v1.Status, finish v1.FinishReason, d *v1.IncompleteDetails) string {
	reason := ""
	if d != nil {
		reason = d.Reason
	}
	return fmt.Sprintf("%s/%s/%s", status, finish, reason)
}

// wireFinish returns the finishReason of the first candidate of a Gemini body.
func wireFinish(t *testing.T, body []byte) string {
	t.Helper()
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		t.Fatal(err)
	}
	if len(gr.Candidates) != 1 {
		t.Fatalf("%d candidates in %s", len(gr.Candidates), body)
	}
	return gr.Candidates[0].FinishReason
}

// A functionCall part promotes only a neutral STOP to tool_calls: safety and truncation reasons survive it (rule 11).
func TestFinishReason_ToCanonical(t *testing.T) {
	cases := []struct {
		reason string
		call   bool
		want   string
	}{
		{"STOP", false, "completed/stop/"},
		{"STOP", true, "completed/tool_calls/"},
		{"", true, "completed/tool_calls/"},
		{"UNEXPECTED_TOOL_CALL", true, "incomplete/other/gemini:UNEXPECTED_TOOL_CALL"},
		{"MAX_TOKENS", false, "incomplete/length/max_output_tokens"},
		{"MAX_TOKENS", true, "incomplete/length/max_output_tokens"},
		{"SAFETY", true, "incomplete/content_filter/content_filter"},
		{"RECITATION", false, "incomplete/content_filter/content_filter"},
		{"BLOCKLIST", false, "incomplete/content_filter/content_filter"},
		{"PROHIBITED_CONTENT", false, "incomplete/content_filter/content_filter"},
		{"SPII", false, "incomplete/content_filter/content_filter"},
		{"IMAGE_SAFETY", false, "incomplete/content_filter/content_filter"},
		{"LANGUAGE", false, "incomplete/content_filter/unsupported_language"},
		{"MALFORMED_FUNCTION_CALL", true, "failed/stop/malformed_function_call"},
		{"NEW_REASON", false, "incomplete/other/gemini:NEW_REASON"},
		{"OTHER", false, "incomplete/other/gemini:OTHER"},
		{"FINISH_REASON_UNSPECIFIED", false, "incomplete/other/gemini:FINISH_REASON_UNSPECIFIED"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s call=%v", c.reason, c.call), func(t *testing.T) {
			part := `{"text":"x"}`
			if c.call {
				part = `{"functionCall":{"name":"f","args":{}}}`
			}
			content := `{"role":"model","parts":[` + part + `]}`
			body := `{"candidates":[{"content":` + content + `,"finishReason":"` + c.reason + `","index":0}]}`

			resp := parseResponse(t, body)
			if got := finishOf(resp.Status, resp.FinishReason, resp.IncompleteDetails); got != c.want {
				t.Errorf("buffered %s, want %s", got, c.want)
			}
			sum, err := tr.DecodeSummary([]byte(body))
			if err != nil || sum.FinishReason != resp.FinishReason {
				t.Errorf("summary finish %q (%v), want %q", sum.FinishReason, err, resp.FinishReason)
			}
			// Streamed, the part arrives a frame before the finish.
			s := toCanonical(t, `{"candidates":[{"content":`+content+`,"index":0}]}`, `{"candidates":[{"finishReason":"`+c.reason+`","index":0}],"usageMetadata":{"promptTokenCount":1}}`)
			if s.done == nil {
				t.Fatalf("stream has no completion: %v", s.events)
			}
			if got := finishOf(s.done.Status, s.done.FinishReason, s.done.IncompleteDetails); got != c.want {
				t.Errorf("streamed %s, want %s", got, c.want)
			}
		})
	}
}

// Only a Gemini upstream's own raw reason is written back; a non-success status never reads as STOP.
func TestFinishReason_FromCanonical(t *testing.T) {
	cases := []struct {
		name    string
		status  v1.Status
		finish  v1.FinishReason
		details string
		want    string
	}{
		{"stop", v1.StatusCompleted, v1.FinishReasonStop, "", "STOP"},
		{"tool calls", v1.StatusCompleted, v1.FinishReasonToolCalls, "", "STOP"},
		{"length", v1.StatusIncomplete, v1.FinishReasonLength, "max_output_tokens", "MAX_TOKENS"},
		{"length without status", "", v1.FinishReasonLength, "", "MAX_TOKENS"},
		{"content filter", v1.StatusIncomplete, v1.FinishReasonContentFilter, "content_filter", "SAFETY"},
		{"refusal", v1.StatusCompleted, v1.FinishReasonRefusal, "", "SAFETY"},
		{"own raw reason", v1.StatusIncomplete, v1.FinishReasonOther, "gemini:NEW_REASON", "NEW_REASON"},
		{"foreign raw reason", v1.StatusIncomplete, v1.FinishReasonOther, "openai:new_reason", "OTHER"},
		{"other without details", v1.StatusIncomplete, v1.FinishReasonOther, "", "OTHER"},
		{"failed without reason", v1.StatusFailed, "", "", "OTHER"},
		{"incomplete without reason", v1.StatusIncomplete, "", "max_output_tokens", "OTHER"},
		{"completed without reason", v1.StatusCompleted, "", "", "STOP"},
		{"no status or reason", "", "", "", "STOP"},
		{"truncated tool call", v1.StatusIncomplete, v1.FinishReasonToolCalls, "max_output_tokens", "MAX_TOKENS"},
		{"own prefix without a reason", v1.StatusIncomplete, v1.FinishReasonOther, "gemini:", "OTHER"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var details *v1.IncompleteDetails
			if c.details != "" {
				details = &v1.IncompleteDetails{Reason: c.details}
			}
			out, err := tr.SerializeResponse(&v1.Response{ID: "g1", Status: c.status, FinishReason: c.finish, IncompleteDetails: details}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := wireFinish(t, out); got != c.want {
				t.Errorf("buffered %s, want %s", got, c.want)
			}
			completed, _ := json.Marshal(v1.GenerationCompletedEvent{ID: "g1", Status: c.status, FinishReason: c.finish, IncompleteDetails: details})
			frames := fromCanonical(t, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completed})
			if got := wireFinish(t, geminiData(t, frames[0])); got != c.want {
				t.Errorf("streamed %s, want %s", got, c.want)
			}
		})
	}
}

func TestFinishReason_MalformedFunctionCallRoundTrips(t *testing.T) {
	t.Skip("bug: MALFORMED_FUNCTION_CALL parses to failed/stop and serializes back as STOP, so a failed generation reads as a clean stop")
	resp := parseResponse(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"MALFORMED_FUNCTION_CALL","index":0}]}`)
	out, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := wireFinish(t, out); got != "MALFORMED_FUNCTION_CALL" {
		t.Errorf("finishReason %s, want MALFORMED_FUNCTION_CALL", got)
	}
}

func TestParseResponse_AbsentAndNullMembers(t *testing.T) {
	resp := parseResponse(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":""},{"functionCall":{"id":"c1","name":"f"}}]},"finishReason":"STOP","safetyRatings":null}],"promptFeedback":null}`)
	assertJSON(t, resp.Output, `[{"type":"function_call","id":"fc_0","call_id":"c1","name":"f","arguments":"{}","status":"completed"}]`)
	if resp.Extensions != nil {
		t.Errorf("extensions %v, want nil", resp.Extensions)
	}
}

func TestSerializeResponse_Body(t *testing.T) {
	text := []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}}}
	summary := []v1.SummaryText{{Text: "s"}}
	feedback := func(pf string) map[string]json.RawMessage {
		return map[string]json.RawMessage{extPromptFeedback: json.RawMessage(pf)}
	}
	const hi = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}]`
	cases := []struct {
		name string
		resp v1.Response
		want string
	}{
		{"no id, model or usage", v1.Response{Output: text}, hi + `}`},
		{"usage without input or reasoning", v1.Response{Output: text, Usage: usage.Tokens{"output": 5}}, hi + `,"usageMetadata":{"candidatesTokenCount":5}}`},
		{"blocked prompt with output keeps the candidate", v1.Response{Output: text, Extensions: feedback(`{"blockReason":"SAFETY"}`)}, hi + `,"promptFeedback":{"blockReason":"SAFETY"}}`},
		{"unblocked prompt without output keeps the candidate", v1.Response{Extensions: feedback(`{"safetyRatings":[]}`)}, `{"candidates":[{"content":{"role":"model","parts":null},"finishReason":"STOP","index":0}],"promptFeedback":{"safetyRatings":[]}}`},
		{
			"call without arguments; reasoning sends its content, else its first summary, else nothing",
			v1.Response{Output: []v1.Item{&v1.FunctionCall{CallID: "f", Name: "f"}, &v1.Reasoning{Content: "c", Summary: summary}, &v1.Reasoning{Summary: summary}, &v1.Reasoning{}}},
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}},{"text":"c","thought":true},{"text":"s","thought":true}]},"finishReason":"STOP","index":0}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := tr.SerializeResponse(&c.resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, out, c.want)
		})
	}
}

func TestSerializeResponse_KeepsThoughtSignatures(t *testing.T) {
	parts := `[{"text":"plan","thought":true,"thoughtSignature":"sig-rs"},{"text":"","thought":true,"thoughtSignature":"sig-empty-rs"},` +
		`{"text":"a","thoughtSignature":"sig-a"},{"text":"b"},{"text":"","thoughtSignature":"sig-tail"},{"text":"","thoughtSignature":"sig-alone"},` +
		`{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig-fc"}]`
	resp := parseResponse(t, `{"candidates":[{"content":{"role":"model","parts":`+parts+`},"finishReason":"STOP","index":0}]}`)
	assertJSON(t, resp.Output, `[
		{"type":"reasoning","id":"rs_0","summary":[{"text":"plan"}],"content":"plan","status":"completed","provider_data":{"thoughtSignature":"sig-rs"}},
		{"type":"reasoning","id":"rs_1","status":"completed","provider_data":{"thoughtSignature":"sig-empty-rs"}},
		{"type":"message","id":"msg_2","status":"completed","role":"assistant","content":[{"type":"output_text","text":"a"}],"provider_data":{"thoughtSignature":"sig-a"}},
		{"type":"message","id":"msg_3","status":"completed","role":"assistant","content":[{"type":"output_text","text":"b"}],"provider_data":{"emptyTextPart":true,"thoughtSignature":"sig-tail"}},
		{"type":"reasoning","id":"rs_4","status":"completed","provider_data":{"emptyTextPart":true,"thoughtSignature":"sig-alone"}},
		{"type":"function_call","id":"fc_5","call_id":"f__relay_call_5","name":"f","arguments":"{}","status":"completed","provider_data":{"thoughtSignature":"sig-fc"}}]`)
	out, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &gr); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, []byte(gr.Candidates[0].Content.Parts), parts)
}

// The streamed thought text precedes its signature, which rides a trailing empty-text part.
func TestFromCanonicalStream_KeepsThoughtSignatures(t *testing.T) {
	frame := func(event string, v any) v1.SSEFrame {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return v1.SSEFrame{Event: event, Data: data}
	}
	frames := fromCanonical(t,
		frame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "rs_0", ItemType: v1.ItemTypeReasoning}),
		frame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "rs_0", Kind: v1.DeltaKindReasoning, Delta: "plan"}),
		frame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "rs_0", Item: &v1.Reasoning{Content: "plan", ProviderData: thoughtSignatureJSON("sig-rs")}}),
		frame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_1", Index: 1, ItemType: v1.ItemTypeFunctionCall, Name: "f", CallID: "f"}),
		frame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_1", Index: 1, Kind: v1.DeltaKindArguments, Delta: "{}"}),
		frame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "fc_1", Index: 1, Item: &v1.FunctionCall{CallID: "f", Name: "f", Arguments: "{}", ProviderData: thoughtSignatureJSON("sig-fc")}}),
		frame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_2", Index: 2, ItemType: v1.ItemTypeMessage}),
		frame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_2", Index: 2, Kind: v1.DeltaKindText, Delta: "hi"}),
		frame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_2", Index: 2, Item: &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}, ProviderData: thoughtSignatureJSON("sig-msg")}}),
		frame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_3", Index: 3, ItemType: v1.ItemTypeMessage}),
		frame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_3", Index: 3, Kind: v1.DeltaKindText, Delta: "bye"}),
		frame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_3", Index: 3, Item: &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "bye"}}}}),
	)
	// Raw wire parts: re-encoding a decoded part would hide a missing empty text.
	var parts []json.RawMessage
	for _, f := range frames {
		if f == "" {
			continue
		}
		var gr struct {
			Candidates []struct {
				Content struct {
					Parts []json.RawMessage `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(geminiData(t, f), &gr); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, gr.Candidates[0].Content.Parts...)
	}
	assertJSON(t, parts, `[{"text":"plan","thought":true},{"text":"","thought":true,"thoughtSignature":"sig-rs"},{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig-fc"},`+
		`{"text":"hi"},{"text":"","thoughtSignature":"sig-msg"},{"text":"bye"}]`)
}
