package openai

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const respCreated = `{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"model":"gpt-5","status":"in_progress","output":[]}}`

func TestResponsesToCanonicalStream(t *testing.T) {
	cases := []struct {
		name   string
		chunks [][]byte
		want   string
	}{
		{
			// A summary-mode model streams its thinking only as summary deltas and closes the item with an empty summary, so the item is backfilled. A repeated response.created starts nothing.
			name: "reasoning summary and text deltas",
			chunks: [][]byte{
				respSSE(ResponsesEventCreated, respCreated),
				respSSE(ResponsesEventCreated, respCreated),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`),
				respSSE(ResponsesEventReasoningSummaryTextDelta, `{"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Let me "}`),
				respSSE(ResponsesEventReasoningSummaryTextDelta, `{"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think."}`),
				respSSE(ResponsesEventReasoningSummaryTextDone, `{"item_id":"rs_1","output_index":0,"summary_index":0,"text":"Let me think."}`),
				respSSE(ResponsesEventReasoningTextDelta, `{"item_id":"rs_1","output_index":0,"content_index":0,"delta":"raw"}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"BLOB"}}`),
			},
			want: `[
				{"event":"generation.created","data":{"id":"resp_1","model":"gpt-5"}},
				{"event":"item.started","data":{"item_id":"rs_1","item_type":"reasoning","index":0}},
				{"event":"item.delta","data":{"item_id":"rs_1","index":0,"kind":"reasoning","delta":"Let me "}},
				{"event":"item.delta","data":{"item_id":"rs_1","index":0,"kind":"reasoning","delta":"think."}},
				{"event":"item.delta","data":{"item_id":"rs_1","index":0,"kind":"reasoning","delta":"raw"}},
				{"event":"item.completed","data":{"item_id":"rs_1","index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"text":"Let me think."}],"content":"raw","provider_data":{"encrypted_content":"BLOB","id":"rs_1"}}}}]`,
		},
		{
			// A call cut off by max_output_tokens can close with broken argument JSON; it must not look runnable.
			name: "truncated function call",
			chunks: [][]byte{
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"search","arguments":""}}`),
				respSSE(ResponsesEventFunctionCallArgumentsDelta, `{"item_id":"fc_1","output_index":1,"delta":"{\"q\":\"go"}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"search","arguments":"{\"q\":\"go","status":"completed"}}`),
				respSSE(ResponsesEventIncomplete, `{"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}}`),
			},
			want: `[
				{"event":"item.started","data":{"item_id":"fc_1","item_type":"function_call","index":1,"name":"search","call_id":"c1"}},
				{"event":"item.delta","data":{"item_id":"fc_1","index":1,"kind":"arguments","delta":"{\"q\":\"go"}},
				{"event":"item.completed","data":{"item_id":"fc_1","index":1,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"search","arguments":"{\"q\":\"go","status":"incomplete"}}},
				{"event":"generation.completed","data":{"id":"resp_1","status":"incomplete","finish_reason":"length","usage":{"input":5,"output":9},"incomplete_details":{"reason":"max_output_tokens"}}}]`,
		},
		{
			// Hosted-tool items have no canonical form, so neither end of their lifecycle may surface; unrecognised events and itemless frames drop too.
			name: "hosted-tool item and unhandled events",
			chunks: [][]byte{
				respSSE(ResponsesEventInProgress, respCreated),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":0,"item":{"type":"web_search_call","id":"ws_0","status":"in_progress"}}`),
				respSSE("response.web_search_call.searching", `{"item_id":"ws_0","output_index":0}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"web_search_call","id":"ws_0","status":"completed"}}`),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":1,"item":null}`),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":1}`),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":1,"item":{"type":"message"}}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":1}`),
				respSSE("response.unknown_future_event", `{"type":"unknown"}`),
				respSSE(ResponsesEventCompleted, `{"response":{"id":"resp_1","status":"completed","service_tier":"priority","output":[{"type":"web_search_call","id":"ws_0","status":"completed"}],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}}`),
			},
			want: `[{"event":"generation.completed","data":{"id":"resp_1","status":"completed","finish_reason":"stop","usage":{"input":384,"output":104,"cache_read":5120,"reasoning":49},"service_tier":"priority"}}]`,
		},
		{
			// The summary an item closes with wins over the streamed one, and an item that streamed none gets none. A response.created without a response object still opens the generation.
			name: "reasoning items closing with and without a summary",
			chunks: [][]byte{
				respSSE(ResponsesEventCreated, `{"type":"response.created"}`),
				respSSE(ResponsesEventReasoningSummaryTextDelta, `{"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"ab"}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]}}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":1,"item":{"type":"reasoning","id":"rs_2","summary":[]}}`),
			},
			want: `[
				{"event":"generation.created","data":{"id":"","model":""}},
				{"event":"item.delta","data":{"item_id":"rs_1","index":0,"kind":"reasoning","delta":"ab"}},
				{"event":"item.completed","data":{"item_id":"rs_1","index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"text":"a"},{"text":"b"}]}}},
				{"event":"item.completed","data":{"item_id":"rs_2","index":1,"item":{"type":"reasoning","id":"rs_2"}}}]`,
		},
		{
			// Only a call that closed as completed is second-guessed, and only when it has arguments to check.
			name: "function calls keep the status they closed with; a tool result item starts under its own type",
			chunks: [][]byte{
				respSSE(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{\"q\":","status":"in_progress"}}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"c2","name":"f","arguments":"{\"q\":"}}`),
				respSSE(ResponsesEventOutputItemDone, `{"output_index":2,"item":{"type":"function_call","id":"fc_3","call_id":"c3","name":"f","arguments":"","status":"completed"}}`),
				respSSE(ResponsesEventOutputItemAdded, `{"output_index":3,"item":{"type":"function_call_output","id":"fco_1","call_id":"c1"}}`),
			},
			want: `[
				{"event":"item.completed","data":{"item_id":"fc_1","index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{\"q\":","status":"in_progress"}}},
				{"event":"item.completed","data":{"item_id":"fc_2","index":1,"item":{"type":"function_call","id":"fc_2","call_id":"c2","name":"f","arguments":"{\"q\":"}}},
				{"event":"item.completed","data":{"item_id":"fc_3","index":2,"item":{"type":"function_call","id":"fc_3","call_id":"c3","name":"f","arguments":"","status":"completed"}}},
				{"event":"item.started","data":{"item_id":"fco_1","item_type":"function_call_output","index":3,"call_id":"c1"}}]`,
		},
		{
			name: "content filter after a refusal delta",
			chunks: [][]byte{
				respSSE(ResponsesEventRefusalDelta, `{"item_id":"msg_1","output_index":0,"content_index":0,"delta":"I can't"}`),
				respSSE(ResponsesEventRefusalDone, `{"item_id":"msg_1","output_index":0,"content_index":0,"refusal":"I can't"}`),
				respSSE(ResponsesEventIncomplete, `{"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}}`),
			},
			want: `[
				{"event":"item.delta","data":{"item_id":"msg_1","index":0,"kind":"text","delta":"I can't"}},
				{"event":"generation.completed","data":{"id":"resp_1","status":"incomplete","finish_reason":"content_filter","incomplete_details":{"reason":"content_filter"}}}]`,
		},
		{
			name:   "failed",
			chunks: [][]byte{respSSE(ResponsesEventFailed, `{"response":{"id":"resp_1","status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`)},
			want:   `[{"event":"generation.completed","data":{"id":"resp_1","status":"failed","usage":{"input":3}}}]`,
		},
		{
			name:   "failed without a response object",
			chunks: [][]byte{respSSE(ResponsesEventFailed, `{"response":null}`)},
			want:   `[{"event":"error","data":{"code":"response_failed","message":"response failed"}}]`,
		},
		{
			name:   "error",
			chunks: [][]byte{respSSE(ResponsesEventError, `{"type":"error","code":"rate_limit_exceeded","message":"slow down","param":"model"}`)},
			want:   `[{"event":"error","data":{"code":"rate_limit_exceeded","message":"slow down"}}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respJSONEq(t, respStream(t, ResponsesTranslator{}.NewToCanonicalStream(), tc.chunks...), tc.want)
		})
	}
}

func TestResponsesToCanonicalStream_ToolResultItemKeepsItsID(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(),
		respSSE(ResponsesEventOutputItemDone, `{"output_index":0,"item":{"type":"function_call_output","id":"fco_1","call_id":"c1","output":"ok"}}`))
	respJSONEq(t, frames, `[{"event":"item.completed","data":{"item_id":"fco_1","index":0,"item":{"type":"function_call_output","call_id":"c1","output":"ok"}}}]`)
}

func TestResponsesToCanonicalStream_CustomToolResultItemKeepsItsID(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(),
		respSSE(ResponsesEventOutputItemDone, `{"output_index":1,"item":{"type":"custom_tool_call_output","id":"ctco_1","call_id":"c1","output":"ok"}}`))
	respJSONEq(t, frames, `[{"event":"item.completed","data":{"item_id":"ctco_1","index":1,"item":{"type":"function_call_output","call_id":"c1","output":"ok"}}}]`)
}

func TestResponsesToCanonicalStream_FailedKeepsError(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewToCanonicalStream(),
		respSSE(ResponsesEventFailed, `{"response":{"id":"resp_1","status":"failed","output":[],"error":{"code":"server_error","message":"boom"}}}`))
	respJSONEq(t, respData(t, frames, v1.EventError), `{"code":"server_error","message":"boom"}`)
}

// respTerminal returns a Responses terminal frame's response object without its wall-clock created_at.
func respTerminal(t *testing.T, data json.RawMessage) map[string]any {
	t.Helper()
	var ev struct {
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil || ev.Response == nil {
		t.Fatalf("no response object: %v: %s", err, data)
	}
	delete(ev.Response, "created_at")
	return ev.Response
}

// Deltas for an item never started and completions of one never opened render nothing.
func TestResponsesFromCanonicalStream_Reasoning(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewFromCanonicalStream(),
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "gen_1", Model: "m"}),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "rs_1", ItemType: v1.ItemTypeReasoning}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "rs_1", Kind: v1.DeltaKindReasoning, Delta: "221 = "}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "unknown", Kind: v1.DeltaKindText, Delta: "dropped"}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "rs_1", Kind: v1.DeltaKindReasoning, Delta: "13 * 17"}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "rs_1", Item: &v1.Reasoning{ID: "rs_1", Content: "221 = 13 * 17", Status: v1.StatusCompleted, ProviderData: json.RawMessage(`{"encrypted_content":"BLOB"}`)}}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "never_started", Item: &v1.Message{Role: v1.RoleAssistant}}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "gen_1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop, Usage: usage.Tokens{"input": 4, "output": 6, "reasoning": 5}}),
		canonicalChunk(v1.EventError, v1.ErrorEvent{Code: "overloaded", Message: "try later"}),
	)
	want := []string{ResponsesEventCreated, ResponsesEventInProgress, ResponsesEventOutputItemAdded, ResponsesEventReasoningTextDelta, ResponsesEventReasoningTextDelta,
		ResponsesEventReasoningTextDone, ResponsesEventOutputItemDone, ResponsesEventCompleted, ResponsesEventError}
	if got := respEvents(frames); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	const item = `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"221 = 13 * 17"}],"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}],"encrypted_content":"BLOB","status":"completed"}`
	for i, want := range []string{
		`{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"status":"in_progress"}}`,
		`{"type":"response.reasoning_text.delta","sequence_number":3,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"221 = "}`,
		`{"type":"response.reasoning_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"13 * 17"}`,
		`{"type":"response.reasoning_text.done","sequence_number":5,"item_id":"rs_1","output_index":0,"content_index":0,"text":"221 = 13 * 17"}`,
		`{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":` + item + `}`,
	} {
		respJSONEq(t, frames[i+2].Data, want)
	}
	respJSONEq(t, respTerminal(t, frames[7].Data), `{"id":"gen_1","object":"response","model":"m","status":"completed","output":[`+item+`],
		"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":5}},
		"error":null,"incomplete_details":null,"instructions":null,"temperature":null,"top_p":null,"parallel_tool_calls":false,"tools":[],"metadata":{},"tool_choice":"auto"}`)
	respJSONEq(t, frames[8].Data, `{"type":"error","sequence_number":8,"code":"overloaded","message":"try later"}`)
}

// A producer that sends no call id at item.started still yields one on the argument deltas: the item id stands in. The completed item's call id, name and arguments then win over what the stream carried.
func TestResponsesFromCanonicalStream_CallIDFallback(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewFromCanonicalStream(),
		canonicalChunk(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_1", ItemType: v1.ItemTypeFunctionCall, Name: "search"}),
		canonicalChunk(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_1", Kind: v1.DeltaKindArguments, Delta: "{}"}),
		canonicalChunk(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "fc_1", Item: &v1.FunctionCall{ID: "fc_1", CallID: "call_9", Name: "lookup", Arguments: `{"q":1}`, Status: v1.StatusCompleted}}),
	)
	respJSONEq(t, respData(t, frames, ResponsesEventFunctionCallArgumentsDelta), `{"type":"response.function_call_arguments.delta","sequence_number":1,"item_id":"fc_1","output_index":0,"call_id":"fc_1","delta":"{}"}`)
	respJSONEq(t, respData(t, frames, ResponsesEventFunctionCallArgumentsDone), `{"type":"response.function_call_arguments.done","sequence_number":2,"item_id":"fc_1","output_index":0,"call_id":"call_9","arguments":"{\"q\":1}"}`)
	respJSONEq(t, respData(t, frames, ResponsesEventOutputItemDone), `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"lookup","arguments":"{\"q\":1}","status":"completed"}}`)
}

// Every event object gets its type and sequence number; data that is not a JSON object is left alone.
func TestResponsesFromCanonicalStream_StampEnvelope(t *testing.T) {
	for data, want := range map[string]string{
		`{}`:      `{"type":"e","sequence_number":0}`,
		`{"a":1}`: `{"type":"e","sequence_number":0,"a":1}`,
		`x`:       `x`,
		`[1]`:     `[1]`,
		`[1}`:     `[1}`,
		`{1]`:     `{1]`,
		`}1}`:     `}1}`,
		`{1~`:     `{1~`,
	} {
		if got := string((&canonicalToResponsesStream{}).stampEnvelope("e", []byte(data))); got != want {
			t.Errorf("stampEnvelope(%s) = %s, want %s", data, got, want)
		}
	}
}

func TestSplitSSEFrames(t *testing.T) {
	cases := map[string][]string{
		"event: a\ndata: 1\n\nevent: b\ndata: 2\n\n": {"event: a\ndata: 1\n\n", "event: b\ndata: 2\n\n"},
		"\n\ndata: 1\n\n\n\ndata: 2":                 {"data: 1\n\n", "data: 2\n\n"},
		"data: 1\n\n \n":                             {"data: 1\n\n"},
		"":                                           nil,
	}
	for in, want := range cases {
		var got []string
		for _, f := range splitSSEFrames([]byte(in)) {
			got = append(got, string(f))
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("splitSSEFrames(%q) (-want +got):\n%s", in, diff)
		}
	}
	frames := splitResponsesSSEFrames([]byte(": ping\n\nevent: a\ndata: 1\n\n"))
	if diff := cmp.Diff([]ResponsesSSEFrame{{Event: "a", Data: []byte("1")}}, frames); diff != "" {
		t.Errorf("splitResponsesSSEFrames skips a frame without data (-want +got):\n%s", diff)
	}
	if got := string(ResponsesSSEFrame{Data: []byte("1")}.Bytes()); got != "data: 1\n\n" {
		t.Errorf("a frame without an event name writes %q, want the data line alone", got)
	}
}

func TestComposedStream_PropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	fail := func([]byte) ([]byte, error) { return nil, boom }
	pass := func(b []byte) ([]byte, error) { return b, nil }
	for name, c := range map[string]*ComposedStream{"to canonical": NewComposedStream(fail, pass), "from canonical": NewComposedStream(pass, fail)} {
		if frames, err := c.Translate([]byte("event: a\ndata: 1\n\n")); !errors.Is(err, boom) {
			t.Errorf("%s: err = %v (frames %v), want boom", name, err, frames)
		}
	}
}

// Responses ends a failed generation with response.failed; response.completed tells the client the turn succeeded.
func TestResponsesFromCanonicalStream_Failed(t *testing.T) {
	frames := respStream(t, ResponsesTranslator{}.NewFromCanonicalStream(),
		canonicalChunk(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "gen_1", Model: "m"}),
		canonicalChunk(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "gen_1", Status: v1.StatusFailed}),
	)
	if got := respTerminal(t, respData(t, frames, ResponsesEventFailed))["status"]; got != "failed" {
		t.Errorf("response.failed status = %v, want failed", got)
	}
}

func TestParseResponsesSSEChunk(t *testing.T) {
	cases := []struct {
		name      string
		chunk     string
		wantEvent string
		wantData  string
		wantOK    bool
	}{
		{name: "single line", chunk: "event: response.created\ndata: {\"a\":1}\n\n", wantEvent: "response.created", wantData: `{"a":1}`, wantOK: true},
		{name: "multi-line data", chunk: "event: response.created\ndata: {\"a\":\ndata: 1}\n\n", wantEvent: "response.created", wantData: "{\"a\":\n1}", wantOK: true},
		{name: "CRLF", chunk: "event: response.created\r\ndata: {\"a\":\r\ndata: 1}\r\n\r\n", wantEvent: "response.created", wantData: "{\"a\":\n1}", wantOK: true},
		{name: "CR", chunk: "event: response.created\rdata: {\"a\":1}\r\r", wantEvent: "response.created", wantData: `{"a":1}`, wantOK: true},
		{name: "comment lines", chunk: ": ping\nevent: response.created\n: data: {\"b\":2}\ndata: {\"a\":1}\n\n", wantEvent: "response.created", wantData: `{"a":1}`, wantOK: true},
		{name: "several events yield the first", chunk: "event: response.created\ndata: {\"a\":1}\n\nevent: response.completed\ndata: {\"a\":2}\n\n", wantEvent: "response.created", wantData: `{"a":1}`, wantOK: true},
		{name: "comment only", chunk: ": ping\n\n"},
		{name: "event without data", chunk: "event: response.created\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, data, ok := ParseResponsesSSEChunk([]byte(tc.chunk))
			if event != tc.wantEvent || string(data) != tc.wantData || ok != tc.wantOK {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", event, data, ok, tc.wantEvent, tc.wantData, tc.wantOK)
			}
		})
	}
}
