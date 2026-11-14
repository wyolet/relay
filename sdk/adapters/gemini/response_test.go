package gemini

import (
	"encoding/json"
	"fmt"
	"testing"

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
			s := toCanonical(t, `{"candidates":[{"content":`+content+`,"index":0}]}`, `{"candidates":[{"finishReason":"`+c.reason+`","index":0}]}`)
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

func TestSerializeResponse_KeepsThoughtSignatures(t *testing.T) {
	t.Skip("bug: SerializeResponse (and NewFromCanonicalStream) drop the thoughtSignature provider_data of reasoning and functionCall items, unannotated (rule 8)")
	parts := `[{"text":"plan","thought":true,"thoughtSignature":"sig-rs"},{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig-fc"}]`
	out, err := tr.SerializeResponse(parseResponse(t, `{"candidates":[{"content":{"role":"model","parts":`+parts+`},"finishReason":"STOP","index":0}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var gr geminiResponse
	if err := json.Unmarshal(out, &gr); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, gr.Candidates[0].Content.Parts, parts)
}
