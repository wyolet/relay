package inference

// Cross-shape smoke: a real CC-inbound request routed to an openai_responses
// upstream, driven end-to-end through Dispatch against an httptest upstream with
// the REAL CC + Responses translators (the other dispatch tests use stub
// translators, which cannot catch cross-shape serialization faults). These
// assert BOTH directions of the chain:
//   - the Responses request body relay produced for the upstream
//   - the CC response body relay returned to the caller
// covering tools, reasoning round-trip injection, usage totals, and streaming.

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// buildRealCrossShapeRegistry registers the OpenAI CC + Responses + canonical
// specs with their real translators (not stubV1Translator), so SerializeRequest
// / ParseResponse actually run.
func buildRealCrossShapeRegistry() *adapter.Registry {
	cc := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  productionSpec(adapters.OpenAI).Translator,
	}).Build()
	responses := (&adapter.Spec{
		Name:        adapters.OpenAIResponses,
		DefaultPath: "/v1/responses",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  productionSpec(adapters.OpenAIResponses).Translator,
		IsNativePath: func(b *binding.Binding, h *host.Host) bool {
			return b.Spec.Adapter == adapters.OpenAI && h.Meta.Name == "openai"
		},
	}).Build()
	canonical := (&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build()
	return adapter.NewRegistry(cc, responses, canonical)
}

// crossShapeCatalog builds the standard dispatch fixture with the model bound to
// the openai_responses adapter on a non-"openai" host (so IsNativePath is false
// → the cross-shape canonical chain runs, not byte-pass), pointed at upstreamURL.
func crossShapeCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	cat, pr := buildDispatchCatalog(t, "groq", adapters.OpenAIResponses)
	pointHostAt(t, cat, upstreamURL)
	return cat, pr
}

func buildCrossShapeDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, buildRealCrossShapeRegistry())
	return d
}

// A realistic buffered Responses upstream reply: a reasoning item (with the
// encrypted blob), a function_call, and usage with cached + reasoning tokens.
const responsesBufferedReply = `{"id":"resp_1","object":"response","created_at":1700000000,"model":"test-model","status":"completed","output":[` +
	`{"type":"reasoning","id":"rs_1","encrypted_content":"BLOB","summary":[],"status":"completed"},` +
	`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}","status":"completed"}` +
	`],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}`

// A multi-turn CC request (system + user + a prior assistant tool call + tool result + tool definitions) through the cross-shape chain to a Responses upstream and back.
func TestCrossShape_CCtoResponses_Buffered(t *testing.T) {
	upURL, last := answeringUpstream(t, "application/json", responsesBufferedReply)
	cat, pr := crossShapeCatalog(t, upURL)
	d := buildCrossShapeDeps(t, cat)

	ccReq := `{"model":"test-model","messages":[` +
		`{"role":"system","content":"be helpful"},` +
		`{"role":"user","content":"weather in SF?"},` +
		`{"role":"assistant","content":"let me check","tool_calls":[{"id":"call_0","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_0","content":"72F and sunny"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],"stream":false}`
	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(ccReq), ModelName: "test-model"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	path, upReq := last()
	if path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", path)
	}
	// Content part types follow the role, the tool result pairs with its call by call_id, the system message becomes instructions, and reasoning stays stateless (store off, encrypted content included).
	wantJSON(t, "Responses request sent upstream", upReq, `{
		"model": "test-model",
		"instructions": "be helpful",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "weather in SF?"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "let me check", "annotations": [], "logprobs": []}]},
			{"type": "function_call", "call_id": "call_0", "name": "get_weather", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_0", "output": "72F and sunny"}
		],
		"tools": [{"type": "function", "name": "get_weather", "description": "w", "parameters": {"type": "object"}}],
		"store": false,
		"include": ["reasoning.encrypted_content"]
	}`)
	// Reasoning tokens are a detail of the completion count, not added to the total.
	wantJSON(t, "Chat Completions response returned", w.Body.String(), `{
		"id": "resp_1", "object": "chat.completion", "created": 1700000000, "model": "test-model",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": null,
			"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}]
		}}],
		"usage": {
			"prompt_tokens": 5504, "completion_tokens": 104, "total_tokens": 5608,
			"prompt_tokens_details": {"cached_tokens": 5120}, "completion_tokens_details": {"reasoning_tokens": 49}
		}
	}`)
}

// A realistic streaming Responses upstream reply (text deltas + terminal usage).
const responsesStreamReply = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"test-model","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"test-model","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`

// A streaming CC request through the chain: upstream Responses SSE to canonical to CC SSE, ending in the terminal chunk and [DONE].
func TestCrossShape_CCtoResponses_Streaming(t *testing.T) {
	upURL, last := answeringUpstream(t, "text/event-stream", responsesStreamReply)
	cat, pr := crossShapeCatalog(t, upURL)
	d := buildCrossShapeDeps(t, cat)

	w := dispatch(d, pr, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ModelName: "test-model",
		Stream:    true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	_, upReq := last()
	wantJSON(t, "Responses request sent upstream", upReq, `{
		"model": "test-model", "stream": true, "store": false, "include": ["reasoning.encrypted_content"],
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}]
	}`)
	chunk := func(delta, finish, usage string) string {
		return `data: {"id":"resp_1","object":"chat.completion.chunk","created":0,"model":"test-model","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]` + usage + "}\n\n"
	}
	want := chunk(`{"role":"assistant"}`, "null", "") + chunk(`{"content":"Hello"}`, "null", "") + chunk(`{"content":" world"}`, "null", "") +
		chunk(`{}`, `"stop"`, `,"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`) + "data: [DONE]\n\n"
	if got := string(clockFields.ReplaceAll(w.Body.Bytes(), []byte(`"$1":0`))); got != want {
		t.Errorf("Chat Completions stream\n got %q\nwant %q", got, want)
	}
}
