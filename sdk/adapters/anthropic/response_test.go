package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// respBody is a Messages response with the fixed envelope members and the given ones.
func respBody(members string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"` + testModel + `",` + members + `}`
}

// response is the canonical form of an end_turn respBody with no content, edited.
func response(edit func(*v1.Response)) *v1.Response {
	r := &v1.Response{ID: "msg_1", Object: "response", Model: testModel, Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}
	edit(r)
	return r
}

func outputText(id, text string) *v1.Message {
	return &v1.Message{ID: id, Status: v1.StatusCompleted, Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: text}}}
}

var responseCases = []struct {
	name string
	dir  direction
	wire string
	resp *v1.Response
}{
	{
		name: "text with usage",
		wire: respBody(`"content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}`),
		resp: response(func(r *v1.Response) {
			r.Output = []v1.Item{outputText("msg_0", "Hello!")}
			r.Usage = usage.Tokens{"input": 10, "output": 5}
		}),
	},
	{
		name: "text then tool call",
		wire: respBody(`"content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"x"}}],"stop_reason":"tool_use"`),
		resp: response(func(r *v1.Response) {
			r.FinishReason = v1.FinishReasonToolCalls
			r.Output = []v1.Item{
				outputText("msg_0", "calling"),
				&v1.FunctionCall{ID: "fc_1", CallID: "toolu_1", Name: "search", Arguments: `{"q":"x"}`, Status: v1.StatusCompleted},
			}
		}),
	},
	{
		name: "tool call without input",
		dir:  parseOnly,
		wire: respBody(`"content":[{"type":"tool_use","id":"toolu_1","name":"now"}],"stop_reason":"tool_use"`),
		resp: response(func(r *v1.Response) {
			r.FinishReason = v1.FinishReasonToolCalls
			r.Output = []v1.Item{&v1.FunctionCall{ID: "fc_0", CallID: "toolu_1", Name: "now", Arguments: `{}`, Status: v1.StatusCompleted}}
		}),
	},
	{
		name: "empty and unparseable call arguments",
		dir:  serializeOnly,
		wire: respBody(`"content":[{"type":"tool_use","id":"t1","name":"f","input":{}},{"type":"tool_use","id":"t2","name":"g","input":{"_raw":"{\"a\":"}}],"stop_reason":"tool_use"`),
		resp: response(func(r *v1.Response) {
			r.FinishReason = v1.FinishReasonToolCalls
			r.Output = []v1.Item{&v1.FunctionCall{CallID: "t1", Name: "f"}, &v1.FunctionCall{CallID: "t2", Name: "g", Arguments: `{"a":`}}
		}),
	},
	{
		name: "signed thinking, with and without text",
		wire: respBody(`"content":[{"type":"thinking","thinking":"plan","signature":"SIG"},{"type":"thinking","thinking":"","signature":"SIG2"},{"type":"text","text":"done"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			plan := signedThinking("plan", "SIG")
			plan.ID, plan.Status = "rs_0", v1.StatusCompleted
			r.Output = []v1.Item{
				plan,
				&v1.Reasoning{ID: "rs_1", Status: v1.StatusCompleted, ProviderData: json.RawMessage(`{"signature":"SIG2","thinking":"","type":"thinking"}`)},
				outputText("msg_2", "done"),
			}
		}),
	},
	{
		name: "redacted thinking keeps its data",
		wire: respBody(`"content":[{"type":"thinking","thinking":"plan","signature":"SIG"},{"type":"redacted_thinking","data":"ENC"},{"type":"text","text":"done"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			plan := signedThinking("plan", "SIG")
			plan.ID, plan.Status = "rs_0", v1.StatusCompleted
			r.Output = []v1.Item{
				plan,
				&v1.Reasoning{ID: "rs_1", Status: v1.StatusCompleted, ProviderData: json.RawMessage(`{"data":"ENC","type":"redacted_thinking"}`)},
				outputText("msg_2", "done"),
			}
		}),
	},
	{
		name: "unsigned thinking",
		wire: respBody(`"content":[{"type":"thinking","thinking":"plan"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			r.Output = []v1.Item{&v1.Reasoning{ID: "rs_0", Content: "plan", Summary: []v1.SummaryText{{Text: "plan"}}, Status: v1.StatusCompleted}}
		}),
	},
	{
		name: "blocks with no canonical item take no output index",
		dir:  parseOnly,
		wire: respBody(`"content":[{"type":"thinking","thinking":""},{"type":"server_tool_use","id":"s1","name":"web_search"},{"type":"future"},{"type":"text","text":"x"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) { r.Output = []v1.Item{outputText("msg_0", "x")} }),
	},
	{
		name: "reasoning from another vendor",
		dir:  serializeOnly,
		wire: respBody(`"content":[{"type":"thinking","thinking":"content"},{"type":"thinking","thinking":"summary"},{"type":"thinking","thinking":"bare"},{"type":"text","text":"in"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			r.Output = []v1.Item{
				&v1.Reasoning{Content: "content", Summary: []v1.SummaryText{{Text: "ignored"}}},
				&v1.Reasoning{Summary: []v1.SummaryText{{Text: "summary"}}, ProviderData: json.RawMessage(`{"type":"thought","thinking":"not ours"}`)},
				&v1.Reasoning{ProviderData: json.RawMessage(`{"type":"other"}`)},
				&v1.Reasoning{ProviderData: json.RawMessage(`{"type":"thinking","thinking":"bare"}`)},
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.TextPart{Text: "in"}}},
			}
		}),
	},
	{
		name: "url citations",
		dir:  parseOnly,
		wire: respBody(`"content":[{"type":"text","text":"cited","citations":[{"type":"url_citation","url":"https://x","title":"T","start_index":1,"end_index":5}]}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			msg := outputText("msg_0", "cited")
			msg.Content[0].(*v1.OutputTextPart).Annotations = []v1.Annotation{&v1.URLCitationAnnotation{URL: "https://x", Title: "T", StartIndex: 1, EndIndex: 5}}
			r.Output = []v1.Item{msg}
		}),
	},
	{
		name: "web search and document citations",
		wire: respBody(`"content":[{"type":"text","text":"cited","citations":[{"type":"web_search_result_location","url":"https://x","title":"T","cited_text":"c","encrypted_index":"E"},{"type":"page_location","cited_text":"c","document_index":1,"start_page_number":2,"end_page_number":3},{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1},{"type":"content_block_location","cited_text":"c","document_index":2,"start_block_index":0,"end_block_index":1},{"type":"search_result_location","cited_text":"c","source":"s","title":"S","search_result_index":0,"start_block_index":0,"end_block_index":1}]}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			msg := outputText("msg_0", "cited")
			msg.Content[0].(*v1.OutputTextPart).Annotations = []v1.Annotation{
				&v1.URLCitationAnnotation{URL: "https://x", Title: "T", ProviderData: json.RawMessage(`{"type":"web_search_result_location","url":"https://x","title":"T","cited_text":"c","encrypted_index":"E"}`)},
				&v1.RawAnnotation{Type: "page_location", JSON: json.RawMessage(`{"type":"page_location","cited_text":"c","document_index":1,"start_page_number":2,"end_page_number":3}`)},
				&v1.RawAnnotation{Type: "char_location", JSON: json.RawMessage(`{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1}`)},
				&v1.RawAnnotation{Type: "content_block_location", JSON: json.RawMessage(`{"type":"content_block_location","cited_text":"c","document_index":2,"start_block_index":0,"end_block_index":1}`)},
				&v1.RawAnnotation{Type: "search_result_location", JSON: json.RawMessage(`{"type":"search_result_location","cited_text":"c","source":"s","title":"S","search_result_index":0,"start_block_index":0,"end_block_index":1}`)},
			}
			r.Output = []v1.Item{msg}
		}),
	},
	{
		// Anthropic has no citation type for these: a web_search_result_location needs the encrypted_index only Anthropic issues.
		name: "citations from other vendors are left out",
		dir:  serializeOnly,
		wire: respBody(`"content":[{"type":"text","text":"cited","citations":[{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1}]},{"type":"text","text":"bare"}],"stop_reason":"end_turn"`),
		resp: response(func(r *v1.Response) {
			cited := outputText("msg_0", "cited")
			cited.Content[0].(*v1.OutputTextPart).Annotations = []v1.Annotation{
				&v1.URLCitationAnnotation{URL: "https://x", Title: "T", StartIndex: 1, EndIndex: 5},
				&v1.URLCitationAnnotation{URL: "https://y", ProviderData: json.RawMessage(`{"encrypted_content":"other vendor"}`)},
				&v1.TextCitationAnnotation{StartIndex: 0, EndIndex: 1},
				&v1.RawAnnotation{Type: "container_file_citation", JSON: json.RawMessage(`{"type":"container_file_citation","file_id":"f"}`)},
				&v1.RawAnnotation{Type: "char_location", JSON: json.RawMessage(`{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1}`)},
			}
			bare := outputText("msg_1", "bare")
			bare.Content[0].(*v1.OutputTextPart).Annotations = []v1.Annotation{&v1.TextCitationAnnotation{StartIndex: 0, EndIndex: 1}}
			r.Output = []v1.Item{cited, bare}
		}),
	},
	{
		name: "cache usage with its ttl split and service tier",
		dir:  parseOnly,
		wire: respBody(`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":300,"cache_read_input_tokens":30,"cache_creation_input_tokens":4000,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500},"service_tier":"priority"}`),
		resp: response(func(r *v1.Response) {
			r.Usage = usage.Tokens{"input": 1000, "output": 300, "cache_read": 30, "cache_creation": 4000, "cache_creation_1h": 1500}
			r.ServiceTier = "priority"
		}),
	},
	{
		name: "cache usage writes the ttl split back",
		dir:  serializeOnly,
		wire: respBody(`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":300,"cache_read_input_tokens":30,"cache_creation_input_tokens":4000,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500}}`),
		resp: response(func(r *v1.Response) {
			r.Usage = usage.Tokens{"input": 1000, "output": 300, "cache_read": 30, "cache_creation": 4000, "cache_creation_1h": 1500}
		}),
	},
	{
		// Without a reported 1h share the split is unknown.
		name: "cache write without a 1h share has no ttl split",
		wire: respBody(`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":4000}`),
		resp: response(func(r *v1.Response) { r.Usage = usage.Tokens{"input": 10, "output": 5, "cache_creation": 4000} }),
	},
	{
		name: "matched stop sequence",
		dir:  parseOnly,
		wire: respBody(`"content":[],"stop_reason":"stop_sequence","stop_sequence":"END"`),
		resp: response(func(r *v1.Response) {
			r.Extensions = map[string]json.RawMessage{"stop_sequence": json.RawMessage(`"END"`)}
		}),
	},
	{
		name: "unknown stop reason keeps its raw value beside the stop sequence",
		dir:  parseOnly,
		wire: respBody(`"content":[],"stop_reason":"new_reason","stop_sequence":"END"`),
		resp: response(func(r *v1.Response) {
			r.Status, r.FinishReason = v1.StatusIncomplete, v1.FinishReasonOther
			r.IncompleteDetails = &v1.IncompleteDetails{Reason: "anthropic:new_reason"}
			r.Extensions = map[string]json.RawMessage{"stop_sequence": json.RawMessage(`"END"`), "anthropic.stop_reason": json.RawMessage(`"new_reason"`)}
		}),
	},
	{
		name: "failed response carries its error",
		dir:  serializeOnly,
		wire: respBody(`"content":[],"stop_reason":"refusal","error":{"type":"server_error","message":"boom"}`),
		resp: response(func(r *v1.Response) {
			r.Status, r.FinishReason = v1.StatusFailed, ""
			r.Error = &v1.Error{Code: "server_error", Message: "boom"}
		}),
	},
}

func TestResponseMapping(t *testing.T) {
	for _, tc := range responseCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dir != serializeOnly {
				got, err := AnthropicTranslator{}.ParseResponse([]byte(tc.wire))
				if err != nil {
					t.Fatal(err)
				}
				if got.CreatedAt <= 0 {
					t.Errorf("created_at = %d, want the parse time", got.CreatedAt)
				}
				got.CreatedAt = 0
				if diff := cmp.Diff(tc.resp, got); diff != "" {
					t.Errorf("(-want +got):\n%s", diff)
				}
			}
			if tc.dir != parseOnly {
				got, err := AnthropicTranslator{}.SerializeResponse(tc.resp, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertJSONEqual(t, got, tc.wire)
			}
		})
	}
}

func TestParseResponse_RejectsInvalidJSON(t *testing.T) {
	if resp, err := (AnthropicTranslator{}).ParseResponse([]byte(`{"id":`)); err == nil {
		t.Errorf("parsed to %s, want an error", mustJSON(resp))
	}
}

var stopReasonCases = []struct {
	dir        direction
	wire       string
	status     v1.Status
	finish     v1.FinishReason
	incomplete string
}{
	{both, "end_turn", v1.StatusCompleted, v1.FinishReasonStop, ""},
	{both, "max_tokens", v1.StatusIncomplete, v1.FinishReasonLength, "max_output_tokens"},
	{both, "tool_use", v1.StatusCompleted, v1.FinishReasonToolCalls, ""},
	{both, "refusal", v1.StatusCompleted, v1.FinishReasonRefusal, ""},
	{both, "pause_turn", v1.StatusIncomplete, "", "pause_turn"},
	// An unknown reason is never a clean stop; its raw value survives the round trip.
	{both, "new_reason", v1.StatusIncomplete, v1.FinishReasonOther, "anthropic:new_reason"},
	{parseOnly, "", v1.StatusCompleted, v1.FinishReasonStop, ""},
	{parseOnly, "stop_sequence", v1.StatusCompleted, v1.FinishReasonStop, ""},
	{serializeOnly, "end_turn", v1.StatusCompleted, "", ""},
	{serializeOnly, "max_tokens", v1.StatusIncomplete, v1.FinishReasonLength, ""},
	// Signals with no Anthropic stop_reason of their own must not read as end_turn.
	{serializeOnly, "refusal", v1.StatusCompleted, v1.FinishReasonContentFilter, ""},
	{serializeOnly, "refusal", v1.StatusIncomplete, v1.FinishReasonOther, ""},
	{serializeOnly, "refusal", v1.StatusIncomplete, v1.FinishReasonOther, "openai:new_reason"},
	{serializeOnly, "refusal", v1.StatusFailed, "", ""},
}

// Each stop reason maps the same way buffered and streamed, in both directions.
func TestStopReasonMapping(t *testing.T) {
	type outcome struct {
		Status     v1.Status
		Finish     v1.FinishReason
		Incomplete string
	}
	reason := func(d *v1.IncompleteDetails) string {
		if d == nil {
			return ""
		}
		return d.Reason
	}
	for _, tc := range stopReasonCases {
		want := outcome{tc.status, tc.finish, tc.incomplete}
		var details *v1.IncompleteDetails
		if tc.incomplete != "" {
			details = &v1.IncompleteDetails{Reason: tc.incomplete}
		}
		if tc.dir != serializeOnly {
			resp, err := AnthropicTranslator{}.ParseResponse([]byte(respBody(`"content":[],"stop_reason":"` + tc.wire + `"`)))
			if err != nil {
				t.Fatal(err)
			}
			if got := (outcome{resp.Status, resp.FinishReason, reason(resp.IncompleteDetails)}); got != want {
				t.Errorf("parse %q = %+v, want %+v", tc.wire, got, want)
			}
			completed := lastGenerationCompleted(t, frames(
				"message_start", `{"message":{"id":"msg_1","model":"m"}}`,
				"message_delta", `{"delta":{"stop_reason":"`+tc.wire+`"}}`,
				"message_stop", `{}`,
			))
			if got := (outcome{completed.Status, completed.FinishReason, reason(completed.IncompleteDetails)}); got != want {
				t.Errorf("stream %q = %+v, want %+v", tc.wire, got, want)
			}
		}
		if tc.dir != parseOnly {
			if got := bufferedStopReason(t, tc.status, tc.finish, details); got != tc.wire {
				t.Errorf("serialize %+v = %q, want %q", want, got, tc.wire)
			}
			if got := streamedStopReason(t, tc.status, tc.finish, details); got != tc.wire {
				t.Errorf("stream serialize %+v = %q, want %q", want, got, tc.wire)
			}
		}
	}
}

func TestStopReason_IncompleteWithoutFinishReasonAgreesBufferedAndStreamed(t *testing.T) {
	for _, details := range []*v1.IncompleteDetails{nil, {Reason: "max_output_tokens"}} {
		buffered := bufferedStopReason(t, v1.StatusIncomplete, "", details)
		streamed := streamedStopReason(t, v1.StatusIncomplete, "", details)
		if buffered != streamed || buffered == "end_turn" {
			t.Errorf("details %+v: buffered %q, streamed %q, want one reason that is not a clean stop", details, buffered, streamed)
		}
	}
}

func TestParseResponse_KeepsDocumentAndSearchCitations(t *testing.T) {
	wire := respBody(`"content":[{"type":"text","text":"cited","citations":[{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1},{"type":"web_search_result_location","url":"https://x","title":"T","cited_text":"c"}]}],"stop_reason":"end_turn"`)
	resp, err := AnthropicTranslator{}.ParseResponse([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Output[0].(*v1.Message).Content[0].(*v1.OutputTextPart).Annotations; len(got) != 2 {
		t.Errorf("annotations = %s, want both citations", mustJSON(got))
	}
}

func bufferedStopReason(t *testing.T, status v1.Status, finish v1.FinishReason, details *v1.IncompleteDetails) string {
	t.Helper()
	out, err := AnthropicTranslator{}.SerializeResponse(&v1.Response{Status: status, FinishReason: finish, IncompleteDetails: details}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.StopReason
}

func streamedStopReason(t *testing.T, status v1.Status, finish v1.FinishReason, details *v1.IncompleteDetails) string {
	t.Helper()
	completed := mustJSON(v1.GenerationCompletedEvent{ID: "msg_1", Status: status, FinishReason: finish, IncompleteDetails: details})
	out := translateAll(t, AnthropicTranslator{}.NewFromCanonicalStream(), frames(v1.EventGenerationCompleted, string(completed)))
	events := decodeSSE(t, out)
	if len(events) != 2 || events[0].Event != "message_delta" || events[1].Event != "message_stop" {
		t.Fatalf("generation.completed → %s, want message_delta then message_stop", out)
	}
	return events[0].Data.(map[string]any)["delta"].(map[string]any)["stop_reason"].(string)
}

func lastGenerationCompleted(t *testing.T, chunks [][]byte) v1.GenerationCompletedEvent {
	t.Helper()
	out := translateAll(t, AnthropicTranslator{}.NewToCanonicalStream(), chunks)
	events := decodeSSE(t, out)
	if len(events) == 0 || events[len(events)-1].Event != v1.EventGenerationCompleted {
		t.Fatalf("last frame is not generation.completed:\n%s", out)
	}
	var completed v1.GenerationCompletedEvent
	if err := json.Unmarshal(mustJSON(events[len(events)-1].Data), &completed); err != nil {
		t.Fatal(err)
	}
	return completed
}
