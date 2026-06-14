package inference

// Cross-shape smoke: a real CC-inbound request routed to an openai_responses
// upstream, driven end-to-end through Dispatch against an httptest upstream with
// the REAL CC + Responses translators (the other dispatch tests use stub
// translators, which is exactly why they never caught the #321→#329 cross-shape
// serialization cluster). These assert BOTH directions of the chain:
//   - the Responses request body relay produced for the upstream
//   - the CC response body relay returned to the caller
// covering tools, reasoning round-trip injection, usage totals, and streaming.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// buildRealCrossShapeRegistry registers the OpenAI CC + Responses + canonical
// specs with their real translators (not stubV1Translator), so SerializeRequest
// / ParseResponse actually run.
func buildRealCrossShapeRegistry() *adapter.Registry {
	cc := (&adapter.Spec{
		Name:         adapters.OpenAI,
		UpstreamPath: "/v1/chat/completions",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:   openai.CCTranslator{},
	}).Build()
	responses := (&adapter.Spec{
		Name:         adapters.OpenAIResponses,
		UpstreamPath: "/v1/responses",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:   openai.ResponsesTranslator{},
		IsNativePath: func(plan *routing.Plan) bool {
			return plan.HostBinding.Spec.Adapter == adapters.OpenAI && plan.Host.Meta.Name == "openai"
		},
	}).Build()
	canonical := (&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build()
	return adapter.NewRegistry(cc, responses, canonical)
}

// crossShapeCatalog builds the standard dispatch fixture with the model bound to
// the openai_responses adapter on a non-"openai" host (so IsNativePath is false
// → the cross-shape canonical chain runs, not byte-pass), pointed at upstreamURL.
func crossShapeCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "groq", adapters.OpenAIResponses)
	h := *cat.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	return cat, rk
}

func buildCrossShapeDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	reg := buildRealCrossShapeRegistry()
	d.Specs = reg
	d.Adapters = reg.AdapterMap()
	return d
}

func mustContain(t *testing.T, label, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s: missing %q\n--- body ---\n%s", label, want, body)
	}
}

func mustNotContain(t *testing.T, label, body, notWant string) {
	t.Helper()
	if strings.Contains(body, notWant) {
		t.Errorf("%s: must NOT contain %q\n--- body ---\n%s", label, notWant, body)
	}
}

// A realistic buffered Responses upstream reply: a reasoning item (with the
// encrypted blob), a function_call, and usage with cached + reasoning tokens.
const responsesBufferedReply = `{"id":"resp_1","object":"response","created_at":1700000000,"model":"test-model","status":"completed","output":[` +
	`{"type":"reasoning","id":"rs_1","encrypted_content":"BLOB","summary":[],"status":"completed"},` +
	`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}","status":"completed"}` +
	`],"usage":{"input_tokens":5504,"input_tokens_details":{"cached_tokens":5120},"output_tokens":104,"output_tokens_details":{"reasoning_tokens":49},"total_tokens":5608}}`

// TestCrossShape_CCtoResponses_Buffered drives a multi-turn CC request (system +
// user + a prior assistant tool-call + tool result + tool definitions) through
// the cross-shape chain to a Responses upstream and back.
func TestCrossShape_CCtoResponses_Buffered(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesBufferedReply))
	}))
	defer up.Close()

	cat, rk := crossShapeCatalog(t, up.URL)
	d := buildCrossShapeDeps(t, cat)

	ccReq := `{"model":"test-model","messages":[` +
		`{"role":"system","content":"be helpful"},` +
		`{"role":"user","content":"weather in SF?"},` +
		`{"role":"assistant","content":"let me check","tool_calls":[{"id":"call_0","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_0","content":"72F and sunny"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],"stream":false}`

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(ccReq),
		ModelName: "test-model",
		Stream:    false,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	upReq := upstreamBody
	mu.Unlock()

	// --- the Responses request relay produced for the upstream ---
	// #321: content part type is role-driven — assistant→output_text, user→input_text.
	mustContain(t, "upstream req", upReq, `"type":"output_text","text":"let me check"`)
	mustContain(t, "upstream req", upReq, `"type":"input_text","text":"weather in SF?"`)
	mustContain(t, "upstream req", upReq, "72F and sunny")               // tool result carried
	mustContain(t, "upstream req", upReq, `"call_id":"call_0"`)          // function_call_output paired
	mustContain(t, "upstream req", upReq, `"name":"get_weather"`)        // tool definition forwarded
	mustContain(t, "upstream req", upReq, `"store":false`)               // #324 stateless reasoning
	mustContain(t, "upstream req", upReq, "reasoning.encrypted_content") // #324 include
	mustNotContain(t, "upstream req", upReq, "stop_sequences")           // #329 nonexistent param
	mustNotContain(t, "upstream req", upReq, "top_k")                    // #329 nonexistent param
	mustNotContain(t, "upstream req", upReq, `"status"`)                 // #327 output-only field stripped

	// --- the CC response relay returned to the caller ---
	ccResp := w.Body.String()
	mustContain(t, "cc resp", ccResp, `"chat.completion"`)  // CC shape, not Responses
	mustContain(t, "cc resp", ccResp, "get_weather")        // tool call surfaced
	mustContain(t, "cc resp", ccResp, `"total_tokens":5608`) // #328 no reasoning double-count
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

// TestCrossShape_CCtoResponses_Streaming drives a streaming CC request through
// the cross-shape chain: upstream Responses SSE → canonical → CC SSE. Guards
// #322 (terminal event/usage) and #323 (the stream flag reaching the upstream).
func TestCrossShape_CCtoResponses_Streaming(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesStreamReply)
	}))
	defer up.Close()

	cat, rk := crossShapeCatalog(t, up.URL)
	d := buildCrossShapeDeps(t, cat)

	ccReq := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(ccReq),
		ModelName: "test-model",
		Stream:    true,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	upReq := upstreamBody
	mu.Unlock()
	mustContain(t, "upstream req", upReq, `"stream":true`) // #323 stream flag propagates

	ccOut := w.Body.String()
	mustContain(t, "cc stream", ccOut, "chat.completion.chunk") // CC streaming shape
	mustContain(t, "cc stream", ccOut, "Hello")                 // text deltas translated
	mustContain(t, "cc stream", ccOut, "world")
	mustContain(t, "cc stream", ccOut, "[DONE]") // #322 terminal frame emitted
}
