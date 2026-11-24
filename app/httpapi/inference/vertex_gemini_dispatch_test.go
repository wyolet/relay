package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
)

// vertexCall is one request a vertex_gemini upstream received.
type vertexCall struct{ Path, Query, Auth, APIKey, Body string }

// vertexUpstream answers every request with reply as contentType, recording each.
func vertexUpstream(t *testing.T, contentType, reply string) (string, func() []vertexCall) {
	t.Helper()
	got := make(chan vertexCall, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- vertexCall{r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Goog-Api-Key"), string(b)}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []vertexCall {
		var out []vertexCall
		for len(got) > 0 {
			out = append(out, <-got)
		}
		return out
	}
}

// vertexDeps is the dispatch fixture with test-model bound through vertex_gemini to a host at upURL whose backend is backend, served by the production specs.
func vertexDeps(t *testing.T, upURL string, backend map[string]string) (Deps, *Principal) {
	t.Helper()
	cat, pr := buildDispatchCatalog(t, "vertex", adapters.VertexGemini)
	setHostSpec(t, cat, host.Spec{BaseURL: upURL, Backend: backend})
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, productionSpecs())
	return d, pr
}

var vertexBackend = map[string]string{"project": "proj-1", "location": "us-central1"}

const vertexModelPath = "/v1/projects/proj-1/locations/us-central1/publishers/google/models/test-model"

// Vertex Gemini replies are the Gemini API's, its stream framed with CRLF.
const (
	vertexReply  = `{"candidates":[{"content":{"parts":[{"text":"Hello."}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10,"trafficType":"ON_DEMAND"},"modelVersion":"gemini-2.5-flash","responseId":"r1"}`
	vertexStream = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}],\"role\":\"model\"},\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":8,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-flash\",\"responseId\":\"r1\"}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo.\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"totalTokenCount\":10,\"trafficType\":\"ON_DEMAND\"},\"modelVersion\":\"gemini-2.5-flash\",\"responseId\":\"r1\"}\r\n\r\n"
)

// OpenAI and Anthropic callers reach a vertex_gemini binding through the Gemini translator, at the Vertex model path with the backend's project and location and a bearer token; a stream goes to streamGenerateContent with alt=sse.
func TestDispatch_VertexGeminiPathAndAuth(t *testing.T) {
	const openaiBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]`
	const anthropicBody = `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	const contents = `"contents":[{"role":"user","parts":[{"text":"hi"}]}]`
	for name, tc := range map[string]struct {
		inbound      adapters.Name
		body         string
		stream       bool
		path, query  string
		upstreamBody string
	}{
		"openai":           {adapters.OpenAI, openaiBody + `}`, false, vertexModelPath + ":generateContent", "", `{` + contents + `}`},
		"openai stream":    {adapters.OpenAI, openaiBody + `,"stream":true}`, true, vertexModelPath + ":streamGenerateContent", "alt=sse", `{` + contents + `}`},
		"anthropic":        {adapters.Anthropic, anthropicBody + `}`, false, vertexModelPath + ":generateContent", "", `{` + contents + `,"generationConfig":{"maxOutputTokens":16}}`},
		"anthropic stream": {adapters.Anthropic, anthropicBody + `,"stream":true}`, true, vertexModelPath + ":streamGenerateContent", "alt=sse", `{` + contents + `,"generationConfig":{"maxOutputTokens":16}}`},
	} {
		t.Run(name, func(t *testing.T) {
			reply, contentType := vertexReply, "application/json"
			if tc.stream {
				reply, contentType = vertexStream, "text/event-stream"
			}
			upURL, calls := vertexUpstream(t, contentType, reply)
			d, pr := vertexDeps(t, upURL, vertexBackend)

			w := dispatch(d, pr, DispatchInput{Inbound: tc.inbound, Body: []byte(tc.body), ModelName: "test-model", Stream: tc.stream})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			got := calls()
			if len(got) != 1 {
				t.Fatalf("upstream calls = %+v, want one", got)
			}
			wantJSON(t, "Gemini request sent upstream", got[0].Body, tc.upstreamBody)
			got[0].Body = ""
			if diff := cmp.Diff(vertexCall{Path: tc.path, Query: tc.query, Auth: "Bearer sk-test"}, got[0]); diff != "" {
				t.Errorf("upstream call (-want +got):\n%s", diff)
			}
		})
	}
}

// A host whose backend leaves {project} unset answers host_misconfigured and calls nothing; a file URL whose media type cannot be told is refused before any call rather than sent without one.
func TestDispatch_VertexGeminiRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		backend map[string]string
		image   string
		status  int
		code    string
	}{
		"backend without project":      {map[string]string{"location": "us-central1"}, "https://example.com/cat.png", http.StatusBadGateway, "host_misconfigured"},
		"image URL with no media type": {vertexBackend, "https://example.com/cat", http.StatusBadRequest, "unsupported_request"},
	} {
		t.Run(name, func(t *testing.T) {
			upURL, calls := vertexUpstream(t, "application/json", vertexReply)
			d, pr := vertexDeps(t, upURL, tc.backend)

			body := `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"` + tc.image + `"}}]}]}`
			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(body), ModelName: "test-model"})

			if w.Code != tc.status || errCode(t, w.Body.Bytes()) != tc.code {
				t.Errorf("status %d: %s; want %d %s", w.Code, w.Body, tc.status, tc.code)
			}
			if got := calls(); len(got) != 0 {
				t.Errorf("upstream was called: %+v", got)
			}
		})
	}
}

// The gemini and vertex_gemini translators differ, so a body serialized for a gemini hop is never sent to a vertex_gemini hop: the route ends at the failed first hop.
func TestDispatch_GeminiDoesNotFailOverToVertexGemini(t *testing.T) {
	firstURL, firstCalls := hopUpstream(t, http.StatusInternalServerError, downReply)
	vertexURL, vertexCalls := vertexUpstream(t, "application/json", vertexReply)
	d, pr := buildHopsDeps(t,
		hopFixture{host: "first", url: firstURL, adapter: adapters.Gemini},
		hopFixture{host: "vertex", url: vertexURL, adapter: adapters.VertexGemini})
	useSpecs(&d, productionSpecs())

	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`), ModelName: "test-model"})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status %d: %s; want the first hop's 500", w.Code, w.Body)
	}
	if got := firstCalls(); len(got) != 1 {
		t.Errorf("gemini hop calls = %+v, want one", got)
	}
	if got := vertexCalls(); len(got) != 0 {
		t.Errorf("vertex_gemini hop was called: %+v", got)
	}
}

// A multi-turn CC request with an image URL, a tool round and structured output, through the real CC and Gemini translators to a vertex_gemini upstream and back.
func TestCrossShape_CCtoVertexGemini_Buffered(t *testing.T) {
	const reply = `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fc-1","name":"get_weather","args":{"city":"Lima"}},"thoughtSignature":"c2ln"}],"role":"model"},"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"promptTokenCount":90,"candidatesTokenCount":20,"thoughtsTokenCount":12,"totalTokenCount":122,"trafficType":"ON_DEMAND"},"modelVersion":"gemini-2.5-flash","responseId":"r2"}`
	upURL, calls := vertexUpstream(t, "application/json", reply)
	d, pr := vertexDeps(t, upURL, vertexBackend)

	ccReq := `{"model":"test-model","messages":[` +
		`{"role":"system","content":"be helpful"},` +
		`{"role":"user","content":[{"type":"text","text":"weather where this was taken?"},{"type":"image_url","image_url":{"url":"https://example.com/street.jpg"}}]},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Quito\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_0","content":"18C"},` +
		`{"role":"user","content":"and Lima?"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"}}}}`
	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(ccReq), ModelName: "test-model"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	got := calls()
	if len(got) != 1 || got[0].Path != vertexModelPath+":generateContent" {
		t.Fatalf("upstream calls = %+v, want one to %s:generateContent", got, vertexModelPath)
	}
	// The image URL carries the mimeType its extension names; the rest is the Gemini API body unchanged.
	wantJSON(t, "Gemini request sent upstream", got[0].Body, `{
		"systemInstruction": {"parts": [{"text": "be helpful"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "weather where this was taken?"}, {"fileData": {"mimeType": "image/jpeg", "fileUri": "https://example.com/street.jpg"}}]},
			{"role": "model", "parts": [{"thoughtSignature": "skip_thought_signature_validator", "functionCall": {"id": "call_0", "name": "get_weather", "args": {"city": "Quito"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"id": "call_0", "name": "get_weather", "response": {"output": "18C"}}}]},
			{"role": "user", "parts": [{"text": "and Lima?"}]}
		],
		"generationConfig": {"responseMimeType": "application/json", "responseJsonSchema": {"type": "object"}},
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "w", "parametersJsonSchema": {"type": "object", "properties": {"city": {"type": "string"}}}}]}]
	}`)
	// Thoughts count beside the candidates, so they join the completion tokens.
	wantJSON(t, "Chat Completions response returned", string(clockFields.ReplaceAll(w.Body.Bytes(), []byte(`"$1":0`))), `{
		"id": "r2", "object": "chat.completion", "created": 0, "model": "gemini-2.5-flash",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": null,
			"tool_calls": [{"id": "fc-1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Lima\"}"}}]
		}}],
		"usage": {"prompt_tokens": 90, "completion_tokens": 32, "total_tokens": 122, "completion_tokens_details": {"reasoning_tokens": 12}}
	}`)
}

// A streaming Anthropic request through the real translators: the CRLF-framed Vertex stream comes back as Anthropic events with the final frame's usage.
func TestCrossShape_AnthropicToVertexGemini_Streaming(t *testing.T) {
	upURL, calls := vertexUpstream(t, "text/event-stream", vertexStream)
	d, pr := vertexDeps(t, upURL, vertexBackend)

	w := dispatch(d, pr, DispatchInput{
		Inbound:   adapters.Anthropic,
		Body:      []byte(`{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		ModelName: "test-model",
		Stream:    true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := calls(); len(got) != 1 || got[0].Path != vertexModelPath+":streamGenerateContent" || got[0].Query != "alt=sse" {
		t.Fatalf("upstream calls = %+v, want one to %s:streamGenerateContent?alt=sse", got, vertexModelPath)
	}

	event := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	want := event("message_start", `{"message":{"content":[],"id":"r1","model":"gemini-2.5-flash","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":0,"output_tokens":0}},"type":"message_start"}`) +
		event("ping", `{"type":"ping"}`) +
		event("content_block_start", `{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`) +
		event("content_block_delta", `{"delta":{"text":"Hel","type":"text_delta"},"index":0,"type":"content_block_delta"}`) +
		event("content_block_delta", `{"delta":{"text":"lo.","type":"text_delta"},"index":0,"type":"content_block_delta"}`) +
		event("content_block_stop", `{"index":0,"type":"content_block_stop"}`) +
		event("message_delta", `{"delta":{"stop_reason":"end_turn","stop_sequence":""},"type":"message_delta","usage":{"input_tokens":8,"output_tokens":2}}`) +
		event("message_stop", `{"type":"message_stop"}`)
	if got := w.Body.String(); got != want {
		t.Errorf("Anthropic stream\n got %q\nwant %q", got, want)
	}
}
