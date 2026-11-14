package openai

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ccChunk is a Chat Completions stream frame carrying one choice delta.
func ccChunk(delta string) string {
	return `data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}`
}

// The recorded chat-* goldens pin the text, tool-call, reasoning_content, refusal and usage streams.
func TestCCToCanonicalStream(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   []string
	}{
		{
			name: "Ollama reasoning then a tool call closes the reasoning",
			frames: []string{
				ccChunk(`{"role":"assistant","reasoning":"hm"}`),
				ccChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]}`),
				"data: [DONE]",
			},
			want: []string{
				`generation.created {"id":"c1","model":"m"}`,
				`item.started {"item_id":"rs_0","item_type":"reasoning","index":0}`,
				`item.delta {"item_id":"rs_0","index":0,"kind":"reasoning","delta":"hm"}`,
				`item.completed {"item_id":"rs_0","index":0,"item":{"type":"reasoning","id":"rs_0","summary":[{"text":"hm"}],"content":"hm","status":"completed","provider_data":{"cc_reasoning_field":"reasoning"}}}`,
				`item.started {"item_id":"call_a","item_type":"function_call","index":1,"name":"f","call_id":"call_a"}`,
				`item.delta {"item_id":"call_a","index":1,"kind":"arguments","delta":"{}"}`,
				`item.completed {"item_id":"call_a","index":1,"item":{"type":"function_call","id":"call_a","call_id":"call_a","name":"f","arguments":"{}","status":"completed"}}`,
				`generation.completed {"id":"c1","status":"completed","finish_reason":"stop"}`,
			},
		},
		{
			name: "interleaved tool calls: a late name is backfilled, truncated arguments mark the call incomplete",
			frames: []string{
				ccChunk(`{"content":"t"}`),
				ccChunk(`{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"f","arguments":"{\"q\":"}}]}`),
				ccChunk(`{"tool_calls":[{"index":1,"function":{"arguments":""}}]}`),
				ccChunk(`{"tool_calls":[{"index":1,"function":{"name":"g","arguments":"{}"}},{"index":0,"function":{"arguments":"\"x\""}}]}`),
				`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
				"data: [DONE]",
			},
			want: []string{
				`generation.created {"id":"c1","model":"m"}`,
				`item.started {"item_id":"msg_0","item_type":"message","index":0}`,
				`item.delta {"item_id":"msg_0","index":0,"kind":"text","delta":"t"}`,
				`item.completed {"item_id":"msg_0","index":0,"item":{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"t"}]}}`,
				`item.started {"item_id":"call_a","item_type":"function_call","index":1,"name":"f","call_id":"call_a"}`,
				`item.delta {"item_id":"call_a","index":1,"kind":"arguments","delta":"{\"q\":"}`,
				`item.started {"item_id":"fc_2","item_type":"function_call","index":2}`,
				`item.delta {"item_id":"fc_2","index":2,"kind":"arguments","delta":"{}"}`,
				`item.delta {"item_id":"call_a","index":1,"kind":"arguments","delta":"\"x\""}`,
				`item.completed {"item_id":"call_a","index":1,"item":{"type":"function_call","id":"call_a","call_id":"call_a","name":"f","arguments":"{\"q\":\"x\"","status":"incomplete"}}`,
				`item.completed {"item_id":"fc_2","index":2,"item":{"type":"function_call","id":"fc_2","call_id":"","name":"g","arguments":"{}","status":"completed"}}`,
				`generation.completed {"id":"c1","status":"incomplete","finish_reason":"length","incomplete_details":{"reason":"max_output_tokens"}}`,
			},
		},
		{
			name:   "a chunk without an id takes one from created",
			frames: []string{`data: {"created":5,"choices":[]}`},
			want:   []string{`generation.created {"id":"resp_5","model":""}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertEvents(t, runStream(t, CCTranslator{}.NewToCanonicalStream(), tc.frames...), tc.want...)
		})
	}
}

func TestCCToCanonicalStream_MissingCreatedStampsNow(t *testing.T) {
	before := time.Now().Unix()
	out := runStream(t, CCTranslator{}.NewToCanonicalStream(), `data: {"choices":[]}`)
	_, data, _ := v1.ParseSSEChunk(out)
	var ev struct{ ID string }
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	digits, _ := strings.CutPrefix(ev.ID, "resp_")
	if sec, err := strconv.ParseInt(digits, 10, 64); err != nil || sec < before || sec > time.Now().Unix() {
		t.Errorf("id = %q, want resp_<now>", ev.ID)
	}
}

// An upstream error frame becomes one canonical error event, and the stream says nothing after it.
func TestCCToCanonicalStream_ErrorFrame(t *testing.T) {
	cases := []struct{ frame, want string }{
		{`{"error":{"message":"overloaded","type":"server_error"}}`, `{"code":"server_error","message":"overloaded"}`},
		{`{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`, `{"code":"rate_limit_exceeded","message":"slow down"}`},
		{`{"error":{"code":429}}`, `{"code":"429","message":"{\"error\":{\"code\":429}}"}`},
		{`{"error":"boom"}`, `{"code":"","message":"{\"error\":\"boom\"}"}`},
		{`{"error":{"message":"x"},"created":"not a number"}`, `{"code":"","message":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.frame, func(t *testing.T) {
			out := runStream(t, CCTranslator{}.NewToCanonicalStream(), ccChunk(`{"content":"a"}`), "data: "+tc.frame, ccChunk(`{"content":"b"}`), "data: [DONE]")
			assertEvents(t, out,
				`generation.created {"id":"c1","model":"m"}`,
				`item.started {"item_id":"msg_0","item_type":"message","index":0}`,
				`item.delta {"item_id":"msg_0","index":0,"kind":"text","delta":"a"}`,
				`error `+tc.want,
			)
		})
	}
}

// assertCCChunks checks the data of each Chat Completions frame in out against want, ignoring the clock-stamped created member.
func assertCCChunks(t *testing.T, out []byte, want ...string) {
	t.Helper()
	var clean []byte
	for _, f := range splitCanonicalFrames(out) {
		_, data, _ := v1.ParseSSEChunk(f)
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			delete(m, "created")
			data = mustJSON(m)
		}
		clean = append(append(append(clean, "data: "...), data...), "\n\n"...)
	}
	for i := range want {
		want[i] = "data " + want[i]
	}
	assertEvents(t, clean, want...)
}

func TestCCFromCanonicalStream(t *testing.T) {
	const head = `"id":"r1","object":"chat.completion.chunk","model":"m"`
	out := runStream(t, CCTranslator{}.NewFromCanonicalStream(),
		"event: generation.created\ndata: {\"id\":\"r1\",\"model\":\"m\"}",
		"event: item.started\ndata: {\"item_id\":\"rs_0\",\"item_type\":\"reasoning\",\"index\":0}",
		"event: item.delta\ndata: {\"item_id\":\"rs_0\",\"index\":0,\"kind\":\"reasoning\",\"delta\":\"hm\"}",
		"event: item.started\ndata: {\"item_id\":\"fc_1\",\"item_type\":\"function_call\",\"index\":1,\"name\":\"f\"}",
		"event: item.delta\ndata: {\"item_id\":\"fc_1\",\"index\":1,\"kind\":\"arguments\",\"delta\":\"{}\"}",
		"event: item.delta\ndata: {\"item_id\":\"fc_9\",\"index\":9,\"kind\":\"arguments\",\"delta\":\"lost\"}",
		"event: item.completed\ndata: {\"item_id\":\"fc_1\",\"index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"{}\"}}",
		"event: error\ndata: {\"code\":\"server_error\",\"message\":\"boom\"}",
	)
	// The tool call goes out under its item id: the real call_id only arrives with item.completed, after the header chunk.
	assertCCChunks(t, out,
		`{`+head+`,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{"reasoning_content":"hm"},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"fc_1","type":"function","function":{"name":"f"}}]},"finish_reason":null}]}`,
		`{`+head+`,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"error":{"message":"boom","type":"server_error","code":"server_error"}}`,
		`[DONE]`,
	)
}
