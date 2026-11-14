package inference

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

func TestForwardHeaders(t *testing.T) {
	in := http.Header{
		"Authorization":          {"Bearer relay-secret"},
		"X-Api-Key":              {"relay-secret"},
		"X-Wr-Api-Key":           {"relay-secret"},
		"X-Wr-Upstream-Host":     {"openai"},
		"X-Wr-Proxy-Mode":        {"Proxy"},
		"X-Wr-Account":           {"acme-ml"},
		"Cookie":                 {"relay_session=abc"},
		"Accept-Encoding":        {"gzip"},
		"Content-Type":           {"application/json"},
		"X-Custom-Caller-Header": {"keep", "both"},
	}
	before := in.Clone()

	// Accept-Encoding goes too: relay negotiates its own upstream coding so the translate path always reads a plain body.
	want := http.Header{"Content-Type": {"application/json"}, "X-Custom-Caller-Header": {"keep", "both"}}
	if diff := cmp.Diff(want, forwardHeaders(in)); diff != "" {
		t.Errorf("headers sent upstream (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, in); diff != "" {
		t.Errorf("forwardHeaders changed the request's own headers (-before +after):\n%s", diff)
	}
}

// Which path a request takes follows from the inbound shape and the routed binding alone: a same-shape or native host gets the body byte for byte at the shape's upstream path, anything else goes through the translators.
func TestDispatch_ShapeSelection(t *testing.T) {
	const translateStub = "stub: not implemented"
	tests := []struct {
		name     string
		host     string
		adapter  adapters.Name
		inbound  adapters.Name
		body     string
		wantPath string // byte-pass: the upstream path that received body verbatim
		status   int
		wantErr  httpapi.APIErrorBody
		wantRow  string // the failure event's message when it is not the envelope's
	}{
		{name: "same shape byte-passes", host: "groq", adapter: adapters.OpenAI, inbound: adapters.OpenAI,
			body: `{"model":"test-model","stream":false}`, wantPath: "/v1/chat/completions"},
		{name: "responses on the native host byte-passes", host: "openai", adapter: adapters.OpenAI, inbound: adapters.OpenAIResponses,
			body: `{"model":"test-model","input":"hi"}`, wantPath: "/v1/responses"},
		{name: "responses on a compatible host translates", host: "ollama-self", adapter: adapters.OpenAI, inbound: adapters.OpenAIResponses,
			body: `{"model":"test-model","input":"hi"}`, status: 400,
			wantErr: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "translate_request", Message: translateStub}},
		{name: "responses on another vendor's host translates", host: "anthropic", adapter: adapters.Anthropic, inbound: adapters.OpenAIResponses,
			body: `{"model":"test-model","input":"hi"}`, status: 400,
			wantErr: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "translate_request", Message: translateStub}},
		{name: "embeddings byte-pass to any compatible host", host: "ollama-self", adapter: adapters.OpenAI, inbound: adapters.OpenAIEmbeddings,
			body: `{"model":"test-model","input":"hello"}`, wantPath: "/v1/embeddings"},
		{name: "embeddings on a host without them", host: "anthropic", adapter: adapters.Anthropic, inbound: adapters.OpenAIEmbeddings,
			body: `{"model":"test-model","input":"hello"}`, status: 400,
			wantErr: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "embeddings_unsupported_host",
				Message: "model test-model is on host anthropic (adapter=anthropic) which does not support OpenAI-compatible embeddings"},
			wantRow: "host does not support OpenAI-compatible embeddings"},
		{name: "canonical body the identity parser rejects", host: "anthropic", adapter: adapters.Anthropic, inbound: adapters.Canonical,
			body: `{"model":"test-model"}`, status: 400,
			wantErr: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "translate_request", Message: "input is required"}},
		{name: "shape with no registered spec", host: "openai", adapter: adapters.OpenAI, inbound: "unregistered",
			body: `{"model":"test-model"}`, status: 500,
			wantErr: httpapi.APIErrorBody{Type: "server_error", Code: "no_spec", Message: "no adapter spec registered for unregistered"}},
		{name: "binding whose adapter has no registered spec", host: "h", adapter: "unregistered", inbound: adapters.OpenAI,
			body: `{"model":"test-model"}`, status: 500,
			wantErr: httpapi.APIErrorBody{Type: "server_error", Code: "no_spec", Message: "no adapter spec registered for upstream unregistered"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type seen struct{ Path, Body string }
			got := make(chan seen, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got <- seen{r.URL.Path, string(b)}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"answer":true}`))
			}))
			defer up.Close()
			cat, pr := buildDispatchCatalog(t, tt.host, tt.adapter)
			pointHostAt(t, cat, up.URL)
			d := buildRunnableDeps(t, cat)
			outcomes := watchOutcomes(&d)

			w := dispatch(d, pr, DispatchInput{Inbound: tt.inbound, Body: []byte(tt.body), ModelName: "test-model"})

			if tt.wantPath == "" {
				wantAnswer(t, w, tt.status, tt.wantErr, map[string]string{HeaderOrigin: "relay"})
				row := tt.wantErr.Message
				if tt.wantRow != "" {
					row = tt.wantRow
				}
				wantOutcome(t, outcomes, outcome{Kind: tt.wantErr.Code, Message: row})
				if len(got) != 0 {
					t.Errorf("a refused request reached the upstream: %+v", <-got)
				}
				return
			}
			if w.Code != http.StatusOK || w.Body.String() != `{"answer":true}` || w.Header().Get(HeaderOrigin) != "upstream" {
				t.Fatalf("response = %d %s (origin %q), want the upstream's answer", w.Code, w.Body, w.Header().Get(HeaderOrigin))
			}
			if diff := cmp.Diff(seen{tt.wantPath, tt.body}, <-got); diff != "" {
				t.Errorf("upstream request (-want +got):\n%s", diff)
			}
			wantOutcome(t, outcomes, outcome{Status: http.StatusOK})
		})
	}
}

// A request refused at routing, before any runner exists, still records what the caller was answered.
func TestDispatch_RoutingRefusal(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	outcomes := watchOutcomes(&d)

	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"no-such-model"}`), ModelName: "no-such-model"})

	want := httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: "host_not_reachable", Message: `model "no-such-model" not found`}
	wantAnswer(t, w, http.StatusNotFound, want, map[string]string{HeaderOrigin: "relay", "X-Should-Retry": "false"})
	wantOutcome(t, outcomes, outcome{Status: http.StatusNotFound, Kind: "model_not_found", Refusal: "host_not_reachable", Message: want.Message})
}

func TestDispatch_NoPrincipal(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	outcomes := watchOutcomes(&d)

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	Dispatch(d, w, r.WithContext(WithClassification(r.Context(), Classification{Mode: ModeNormal})), DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{}`), ModelName: "test-model"})

	wantAnswer(t, w, http.StatusUnauthorized, httpapi.APIErrorBody{Type: "invalid_request_error", Code: "unauthenticated", Message: "missing relay key"}, nil)
	wantOutcome(t, outcomes, outcome{Kind: "unauthenticated", Message: "missing relay key"})
}

func TestInjectRelayUsage(t *testing.T) {
	ru := &pkgrelay.RelayUsage{RequestID: "r1", Attempts: 2}
	const usage = `"relay_usage":{"request_id":"r1","attempts":2}`
	for name, tc := range map[string]struct{ body, want string }{
		"object":             {`{"id":"x","object":"chat.completion"}`, `{"id":"x","object":"chat.completion",` + usage + `}`},
		"empty object":       {`{}`, `{` + usage + `}`},
		"trailing newline":   {"{\"id\":\"x\"}\n", `{"id":"x",` + usage + `}`},
		"space before brace": {"{\"id\":\"x\" \n }", `{"id":"x",` + usage + `}`},
		"array":              {`[1,2,3]`, `[1,2,3]`},
		"bare string":        {`"nope"`, `"nope"`},
		"empty":              {``, ``},
	} {
		if got := string(injectRelayUsage([]byte(tc.body), ru)); got != tc.want {
			t.Errorf("%s: injectRelayUsage(%q) = %q, want %q", name, tc.body, got, tc.want)
		}
	}
	for name, tc := range map[string]struct{ frame, want string }{
		"terminal event":        {"event: generation.completed\ndata: {\"id\":\"g1\"}", "event: generation.completed\ndata: {\"id\":\"g1\"," + usage + "}"},
		"lines after the data":  {"data: {\"id\":\"g1\"}\nid: 7", "data: {\"id\":\"g1\"," + usage + "}\nid: 7"},
		"no data line":          {": keepalive", ": keepalive"},
		"data is not an object": {"data: [DONE]", "data: [DONE]"},
	} {
		if got := string(injectRelayUsageSSE([]byte(tc.frame), ru)); got != tc.want {
			t.Errorf("%s: injectRelayUsageSSE(%q) = %q, want %q", name, tc.frame, got, tc.want)
		}
	}
}

// The reasoning span is stamped from the canonical reasoning events and only from them; the frames pass through untouched.
func TestStreamCanonical_ReasoningSpan(t *testing.T) {
	const created = "event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n"
	const text = "event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"hi\"}\n\n"
	const completed = "event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n"
	const reasoning = "event: item.started\ndata: {\"item_id\":\"r1\",\"item_type\":\"reasoning\",\"index\":0}\n\n" +
		"event: item.delta\ndata: {\"item_id\":\"r1\",\"kind\":\"reasoning\",\"delta\":\"think\"}\n\n" +
		"event: item.completed\ndata: {\"item_id\":\"r1\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"summary\":[{\"text\":\"t\"}]}}\n\n"
	identity := func(b []byte) ([]byte, error) { return b, nil }

	for name, tc := range map[string]struct {
		stream   string
		track    bool
		wantSpan bool
	}{
		"reasoning frames":            {created + reasoning + text + completed, true, true},
		"no reasoning frames":         {created + text + completed, true, false},
		"reasoning frames, untracked": {created + reasoning + text + completed, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Anchored in the past so an elapsed mark is unambiguously above zero.
			lc := lifecycle.NewContext("req-1", "pipeline", time.Now().Add(-time.Millisecond))
			r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
			r = r.WithContext(lifecycle.ContextWith(r.Context(), lc))
			w := httptest.NewRecorder()

			streamCanonical(Deps{Lifecycle: lifecycle.New()}, w, r, io.NopCloser(strings.NewReader(tc.stream)), false, tc.track, streamShapes{}, identity, identity)

			if w.Body.String() != tc.stream {
				t.Errorf("stream = %q, want the frames unchanged", w.Body)
			}
			span := lc.Timing.Reasoning
			if stamped := span.Start > 0 && span.End >= span.Start; stamped != tc.wantSpan || (!tc.wantSpan && (span.Start != 0 || span.End != 0)) {
				t.Errorf("reasoning span = %+v, want stamped=%v", span, tc.wantSpan)
			}
		})
	}
}

// okUpstreamTranslator parses any body into a fixed canonical Response with the flat usage map relay's adapters produce.
type okUpstreamTranslator struct{ stubV1Translator }

func (okUpstreamTranslator) ParseResponse(_ []byte) (*pkgrelay.Response, error) {
	return &pkgrelay.Response{
		Object:       "response",
		Status:       pkgrelay.StatusCompleted,
		Usage:        pkgusage.Tokens{"input": 10, "output": 5},
		FinishReason: pkgrelay.FinishReasonStop,
		ServiceTier:  "priority",
	}, nil
}

// rawUpstreamBody is a vendor body whose usage block nests detail objects, the shape a canonical client's flat usage map cannot decode.
const rawUpstreamBody = `{"id":"x","object":"chat.completion","model":"gpt-oss:120b-cloud",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0},` +
	`"completion_tokens_details":{"reasoning_tokens":0}}}`

// A canonical caller gets canonical or a relay error, never the upstream's vendor bytes.
func TestBufferCanonical(t *testing.T) {
	tests := []struct {
		name     string
		body     io.Reader
		upstream pkgrelay.Translator
		inbound  pkgrelay.Translator
		status   int
		wantErr  httpapi.APIErrorBody
	}{
		{name: "translated", body: strings.NewReader(rawUpstreamBody), upstream: okUpstreamTranslator{}, inbound: pkgrelay.IdentityTranslator{}, status: 200},
		{name: "upstream body cannot be read", body: iotest.ErrReader(errors.New("connection reset")), upstream: okUpstreamTranslator{}, inbound: pkgrelay.IdentityTranslator{}, status: 502,
			wantErr: httpapi.APIErrorBody{Type: "upstream_error", Code: "read_response", Message: "connection reset"}},
		{name: "upstream body does not parse", body: strings.NewReader(rawUpstreamBody), upstream: stubV1Translator{}, inbound: pkgrelay.IdentityTranslator{}, status: 502,
			wantErr: httpapi.APIErrorBody{Type: "upstream_error", Code: "translate_response", Message: "stub: not implemented"}},
		{name: "response does not serialize", body: strings.NewReader(rawUpstreamBody), upstream: okUpstreamTranslator{}, inbound: stubV1Translator{}, status: 500,
			wantErr: httpapi.APIErrorBody{Type: "server_error", Code: "serialize_response", Message: "stub: not implemented"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Deps{}
			outcomes := watchOutcomes(&d)
			lc := lifecycle.NewContext("req", "pipeline", time.Now())
			r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
			r = r.WithContext(lifecycle.ContextWith(r.Context(), lc))
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Length", "999")

			bufferCanonical(d, rec, r, io.NopCloser(tt.body), http.StatusOK, false, nil, tt.upstream, tt.inbound)

			if tt.status != http.StatusOK {
				wantAnswer(t, rec, tt.status, tt.wantErr, map[string]string{HeaderOrigin: "relay"})
				wantOutcome(t, outcomes, outcome{Kind: tt.wantErr.Code, Message: tt.wantErr.Message})
				return
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q: %v", rec.Body, err)
			}
			want := map[string]any{"id": "", "model": "", "created_at": 0.0, "object": "response", "status": "completed", "finish_reason": "stop", "service_tier": "priority", "usage": map[string]any{"input": 10.0, "output": 5.0}}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("canonical body (-want +got):\n%s", diff)
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "" {
				t.Errorf("status %d, Content-Length %q; want 200 and the upstream's length dropped", rec.Code, rec.Header().Get("Content-Length"))
			}
			// The translation already decoded the body; post-flight reuses its Summary instead of reading the tee'd copy again.
			sum, ok := lc.ResponseSummary()
			wantSum := pkgrelay.Summary{Tokens: pkgusage.Tokens{"input": 10, "output": 5}, FinishReason: pkgrelay.FinishReasonStop, ServiceTier: "priority"}
			if diff := cmp.Diff(wantSum, sum); diff != "" || !ok {
				t.Errorf("recorded Summary ok=%v (-want +got):\n%s", ok, diff)
			}
		})
	}
}

// An upstream error reaches the caller with the provider's status and wording; a bodiless one gets relay's envelope under that status.
func TestForwardUpstreamError(t *testing.T) {
	const upstreamErr = `{"error":{"message":"Please use /v1/responses instead.","type":"invalid_request_error","param":"reasoning_effort","code":null}}`
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.Header().Set("Content-Encoding", "gzip")
	rec.Header().Set("Content-Length", "12")
	forwardUpstreamError(rec, io.NopCloser(strings.NewReader(upstreamErr)), http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != upstreamErr {
		t.Errorf("response = %d %s, want the upstream's 400 body verbatim", rec.Code, rec.Body)
	}
	// The body went out decoded, so the upstream's coding and length no longer describe it.
	if diff := cmp.Diff(http.Header{"Content-Type": {"application/json"}}, rec.Header()); diff != "" {
		t.Errorf("headers (-want +got):\n%s", diff)
	}

	for name, body := range map[string]io.Reader{"blank body": strings.NewReader("   "), "unreadable body": iotest.ErrReader(errors.New("reset"))} {
		rec := httptest.NewRecorder()
		forwardUpstreamError(rec, io.NopCloser(body), http.StatusBadGateway)
		t.Run(name, func(t *testing.T) {
			wantAnswer(t, rec, http.StatusBadGateway, httpapi.APIErrorBody{Type: "upstream_error", Code: "upstream_error", Message: "upstream returned status 502 with no body"}, map[string]string{HeaderOrigin: "relay"})
		})
	}
}
