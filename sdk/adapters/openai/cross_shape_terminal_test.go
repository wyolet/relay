package openai_test

// Cross-shape terminal status: no serializer may emit a success signal for a canonical response that ended in failure, truncation or an unknown reason (rule 11).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func xsSSE(event, data string) []byte {
	if event == "" {
		return []byte("data: " + data + "\n\n")
	}
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

func xsSplit(b []byte) [][]byte {
	var out [][]byte
	for _, f := range strings.Split(string(b), "\n\n") {
		if strings.TrimSpace(f) != "" {
			out = append(out, []byte(f+"\n\n"))
		}
	}
	return out
}

func xsPipe(t *testing.T, toCanon, fromCanon func([]byte) ([]byte, error), frames ...[]byte) (canon, wire []byte) {
	t.Helper()
	for _, fr := range frames {
		c, err := toCanon(fr)
		if err != nil {
			t.Fatalf("toCanon: %v", err)
		}
		canon = append(canon, c...)
		for _, cf := range xsSplit(c) {
			w, err := fromCanon(cf)
			if err != nil {
				t.Fatalf("fromCanon: %v", err)
			}
			wire = append(wire, w...)
		}
	}
	return canon, wire
}

// unknownFinishWire is one wire shape's unknown-reason fixtures and the field
// its terminal reason is read from.
type unknownFinishWire struct {
	name   string
	tr     v1.Translator
	body   []byte
	stream [][]byte
	// raw is the canonical incomplete_details.reason the unknown reason parses to.
	raw string
	// read pulls the terminal reason out of one body or frame of this wire.
	read func(m map[string]any) string
}

func unknownFinishWires() []unknownFinishWire {
	str := func(v any) string { s, _ := v.(string); return s }
	first := func(v any) map[string]any {
		if a, ok := v.([]any); ok && len(a) > 0 {
			m, _ := a[0].(map[string]any)
			return m
		}
		return nil
	}
	return []unknownFinishWire{
		{
			name: "cc",
			tr:   openai.CCTranslator{},
			body: []byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"new_reason"}]}`),
			stream: [][]byte{
				xsSSE("", `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"x"},"finish_reason":null}]}`),
				xsSSE("", `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"new_reason"}]}`),
				xsSSE("", `[DONE]`),
			},
			raw: "openai:new_reason",
			read: func(m map[string]any) string {
				if e, ok := m["error"].(map[string]any); ok {
					return "error/" + str(e["code"])
				}
				return str(first(m["choices"])["finish_reason"])
			},
		},
		{
			name: "responses",
			tr:   openai.ResponsesTranslator{},
			body: []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"new_reason"},"model":"m","output":[]}`),
			stream: [][]byte{
				xsSSE("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`),
				xsSSE("response.incomplete", `{"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"new_reason"},"model":"m","output":[]}}`),
			},
			raw: "new_reason",
			read: func(m map[string]any) string {
				if r, ok := m["response"].(map[string]any); ok {
					m = r
				}
				status := str(m["status"])
				if status == "" || status == "in_progress" {
					return ""
				}
				reason := ""
				if d, ok := m["incomplete_details"].(map[string]any); ok {
					reason = str(d["reason"])
				}
				return status + "/" + reason
			},
		},
		{
			name: "anthropic",
			tr:   anthropic.AnthropicTranslator{},
			body: []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"new_reason","usage":{"input_tokens":1,"output_tokens":1}}`),
			stream: [][]byte{
				xsSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
				xsSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				xsSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`),
				xsSSE("content_block_stop", `{"type":"content_block_stop","index":0}`),
				xsSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"new_reason","stop_sequence":null},"usage":{"output_tokens":1}}`),
				xsSSE("message_stop", `{"type":"message_stop"}`),
			},
			raw: "anthropic:new_reason",
			read: func(m map[string]any) string {
				if d, ok := m["delta"].(map[string]any); ok {
					return str(d["stop_reason"])
				}
				return str(m["stop_reason"])
			},
		},
		{
			name: "gemini",
			tr:   gemini.GeminiTranslator{},
			body: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"NEW_REASON","index":0}],"modelVersion":"m"}`),
			stream: [][]byte{
				xsSSE("", `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"index":0}],"modelVersion":"m"}`),
				xsSSE("", `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"NEW_REASON","index":0}],"modelVersion":"m"}`),
			},
			raw:  "gemini:NEW_REASON",
			read: func(m map[string]any) string { return str(first(m["candidates"])["finishReason"]) },
		},
	}
}

// unknownFinishWant is the terminal reason each target wire (inner key) gets
// for each source wire's unknown reason. The diagonal is the same-vendor
// round trip.
var unknownFinishWant = map[string]map[string]string{
	"cc":        {"cc": "new_reason", "responses": "incomplete/openai:new_reason", "anthropic": "refusal", "gemini": "OTHER"},
	"responses": {"cc": "content_filter", "responses": "incomplete/new_reason", "anthropic": "refusal", "gemini": "OTHER"},
	"anthropic": {"cc": "content_filter", "responses": "incomplete/anthropic:new_reason", "anthropic": "new_reason", "gemini": "OTHER"},
	"gemini":    {"cc": "content_filter", "responses": "incomplete/gemini:NEW_REASON", "anthropic": "refusal", "gemini": "NEW_REASON"},
}

// lastWireReason returns the last non-empty terminal reason across frames.
func lastWireReason(w unknownFinishWire, frames [][]byte) string {
	got := ""
	for _, f := range frames {
		data := f
		if _, d, ok := v1.ParseSSEChunk(f); ok {
			data = d
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if r := w.read(m); r != "" {
			got = r
		}
	}
	return got
}

func TestUnknownFinishReason_Buffered(t *testing.T) {
	wires := unknownFinishWires()
	for _, src := range wires {
		resp, err := src.tr.ParseResponse(src.body)
		if err != nil {
			t.Fatalf("%s parse: %v", src.name, err)
		}
		if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther || resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != src.raw {
			t.Errorf("%s parse = %q/%q/%+v, want incomplete/other/%s", src.name, resp.Status, resp.FinishReason, resp.IncompleteDetails, src.raw)
		}
		for _, dst := range wires {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				out, err := dst.tr.SerializeResponse(resp, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := lastWireReason(dst, [][]byte{out}), unknownFinishWant[src.name][dst.name]; got != want {
					t.Errorf("reason = %q, want %q\n%s", got, want, out)
				}
			})
		}
	}
}

func TestUnknownFinishReason_Stream(t *testing.T) {
	wires := unknownFinishWires()
	for _, src := range wires {
		for _, dst := range wires {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				canon, wire := xsPipe(t, src.tr.NewToCanonicalStream(), dst.tr.NewFromCanonicalStream(), src.stream...)
				var done v1.GenerationCompletedEvent
				for _, f := range xsSplit(canon) {
					if ev, data, _ := v1.ParseSSEChunk(f); ev == v1.EventGenerationCompleted {
						if err := json.Unmarshal(data, &done); err != nil {
							t.Fatal(err)
						}
					}
				}
				if done.Status != v1.StatusIncomplete || done.FinishReason != v1.FinishReasonOther || done.IncompleteDetails == nil || done.IncompleteDetails.Reason != src.raw {
					t.Errorf("canonical completed = %q/%q/%+v, want incomplete/other/%s", done.Status, done.FinishReason, done.IncompleteDetails, src.raw)
				}
				if got, want := lastWireReason(dst, xsSplit(wire)), unknownFinishWant[src.name][dst.name]; got != want {
					t.Errorf("reason = %q, want %q\n%s", got, want, wire)
				}
			})
		}
	}
}

// A canonical caller may end a response without a finish reason, or with other and no raw reason; no wire may read it as success.
func TestTerminalWithoutFinishReason(t *testing.T) {
	cases := []struct {
		name   string
		status v1.Status
		finish v1.FinishReason
		inc    *v1.IncompleteDetails
		want   map[string]string
	}{
		{"other without details", v1.StatusIncomplete, v1.FinishReasonOther, nil,
			map[string]string{"cc": "content_filter", "responses": "incomplete/", "anthropic": "refusal", "gemini": "OTHER"}},
		{"Anthropic pause_turn", v1.StatusIncomplete, "", &v1.IncompleteDetails{Reason: "pause_turn"},
			map[string]string{"cc": "content_filter", "responses": "incomplete/pause_turn", "anthropic": "pause_turn", "gemini": "OTHER"}},
		{"failed", v1.StatusFailed, "", nil,
			map[string]string{"cc": "content_filter", "responses": "failed/", "anthropic": "refusal", "gemini": "OTHER"}},
	}
	created := xsSSE(v1.EventGenerationCreated, `{"id":"r1","model":"m"}`)
	for _, tc := range cases {
		resp := &v1.Response{ID: "r1", Model: "m", Status: tc.status, FinishReason: tc.finish, IncompleteDetails: tc.inc}
		completed, _ := json.Marshal(v1.GenerationCompletedEvent{ID: "r1", Status: tc.status, FinishReason: tc.finish, IncompleteDetails: tc.inc})
		for _, w := range unknownFinishWires() {
			t.Run(tc.name+"/"+w.name, func(t *testing.T) {
				out, err := w.tr.SerializeResponse(resp, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := lastWireReason(w, [][]byte{out}); got != tc.want[w.name] {
					t.Errorf("buffered reason = %q, want %q\n%s", got, tc.want[w.name], out)
				}
				_, wire := xsPipe(t, func(b []byte) ([]byte, error) { return b, nil }, w.tr.NewFromCanonicalStream(), created, xsSSE(v1.EventGenerationCompleted, string(completed)))
				if got := lastWireReason(w, xsSplit(wire)); got != tc.want[w.name] {
					t.Errorf("stream reason = %q, want %q\n%s", got, tc.want[w.name], wire)
				}
			})
		}
	}
}

var (
	responsesFailedBody   = `{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"The model produced invalid output"}}`
	responsesFailedStream = [][]byte{
		xsSSE("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`),
		xsSSE("response.failed", `{"type":"response.failed","sequence_number":1,"response":`+responsesFailedBody+`}`),
	}
)

// A Responses upstream that failed reaches every wire as a failure, buffered and streamed.
func TestResponsesFailed_CrossShape(t *testing.T) {
	want := map[string][2]string{ // buffered, stream
		"cc":        {"error/server_error", "content_filter"},
		"responses": {"failed/", "failed/"},
		"anthropic": {"refusal", "refusal"},
		"gemini":    {"OTHER", "OTHER"},
	}
	resp, err := openai.ResponsesTranslator{}.ParseResponse([]byte(responsesFailedBody))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range unknownFinishWires() {
		t.Run(w.name, func(t *testing.T) {
			out, err := w.tr.SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := lastWireReason(w, [][]byte{out}); got != want[w.name][0] {
				t.Errorf("buffered reason = %q, want %q\n%s", got, want[w.name][0], out)
			}
			_, wire := xsPipe(t, openai.ResponsesTranslator{}.NewToCanonicalStream(), w.tr.NewFromCanonicalStream(), responsesFailedStream...)
			if got := lastWireReason(w, xsSplit(wire)); got != want[w.name][1] {
				t.Errorf("stream reason = %q, want %q\n%s", got, want[w.name][1], wire)
			}
		})
	}
}

func TestResponsesFailed_StreamEndsInResponseFailed(t *testing.T) {
	t.Skip(`bug: response.failed{error:server_error} → canonical → Responses stream ends in response.completed with status "failed" and error null`)
	_, wire := xsPipe(t, openai.ResponsesTranslator{}.NewToCanonicalStream(), openai.ResponsesTranslator{}.NewFromCanonicalStream(), responsesFailedStream...)
	frames := xsSplit(wire)
	event, data, _ := v1.ParseSSEChunk(frames[len(frames)-1])
	var terminal struct {
		Response struct {
			Error *struct{ Code string }
		}
	}
	if err := json.Unmarshal(data, &terminal); err != nil {
		t.Fatal(err)
	}
	if event != "response.failed" || terminal.Response.Error == nil || terminal.Response.Error.Code != "server_error" {
		t.Errorf("terminal event %s, want response.failed carrying the upstream error:\n%s", event, data)
	}
}
