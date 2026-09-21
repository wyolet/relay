package inference

// Keepalive on the streamed response paths: when the upstream goes silent mid-stream, relay must keep the connection alive with a frame of the INBOUND shape (Anthropic callers get `ping`, everyone else an SSE comment), and it must never splice one into a half-written frame.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const keepAliveInterval = 50 * time.Millisecond

// upstreamSilence is long enough for several keepalive intervals to elapse.
const upstreamSilence = 300 * time.Millisecond

// keepAliveRegistry registers the CC + Anthropic shapes with their real translators, so the emitted frame is the one that shape actually declares.
func keepAliveRegistry() *adapter.Registry {
	anth := (&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		Auth:        adapter.AuthStrategy{Header: "x-api-key"},
		Translator:  anthropic.AnthropicTranslator{},
	}).Build()
	cc := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  openai.CCTranslator{},
	}).Build()
	canonical := (&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build()
	return adapter.NewRegistry(anth, cc, canonical)
}

func keepAliveCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	h := *cat.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	return cat, rk
}

func keepAliveDeps(t *testing.T, cat *catalog.Catalog, every time.Duration) Deps {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	reg := keepAliveRegistry()
	d.Specs = reg
	d.Adapters = reg.AdapterMap()
	d.StreamKeepAlive = every
	return d
}

// silentGapUpstream streams head, goes quiet for upstreamSilence, then streams tail.
func silentGapUpstream(t *testing.T, head, tail string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = w.Write([]byte(head))
		f.Flush()
		time.Sleep(upstreamSilence)
		_, _ = w.Write([]byte(tail))
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertWellFormedSSE fails if any frame carries a line that is not an SSE field or comment — the shape a keepalive spliced into a half-written frame would produce.
func assertWellFormedSSE(t *testing.T, body string) {
	t.Helper()
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if line == "" {
				continue
			}
			switch {
			case strings.HasPrefix(line, ":"),
				strings.HasPrefix(line, "event:"),
				strings.HasPrefix(line, "data:"),
				strings.HasPrefix(line, "id:"),
				strings.HasPrefix(line, "retry:"):
			default:
				t.Fatalf("corrupt SSE line %q in frame %q", line, frame)
			}
		}
	}
}

const anthropicStreamHead = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

`

const anthropicStreamTail = `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// Byte-pass (Anthropic in, Anthropic upstream): the gap is filled with the shape's own `ping`, not a comment.
func TestDispatch_KeepAlive_BytePass_EmitsPing(t *testing.T) {
	up := silentGapUpstream(t, anthropicStreamHead, anthropicStreamTail)
	cat, rk := keepAliveCatalog(t, up.URL)
	d := keepAliveDeps(t, cat, keepAliveInterval)

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Anthropic,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ModelName: "test-model",
		Stream:    true,
	})

	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if got := strings.Count(body, "event: ping"); got < 2 {
		t.Fatalf("ping frames during a %v upstream gap = %d, want >= 2\n%s", upstreamSilence, got, body)
	}
	mustContain(t, "byte-pass stream", body, "Hello")
	mustContain(t, "byte-pass stream", body, "message_stop")
	assertWellFormedSSE(t, body)
}

// Cross-shape (CC in, Anthropic upstream): CC has no idle frame of its own, so the caller gets the SSE comment.
func TestDispatch_KeepAlive_CrossShape_EmitsComment(t *testing.T) {
	up := silentGapUpstream(t, anthropicStreamHead, anthropicStreamTail)
	cat, rk := keepAliveCatalog(t, up.URL)
	d := keepAliveDeps(t, cat, keepAliveInterval)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ModelName: "test-model",
		Stream:    true,
	})

	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if got := strings.Count(body, string(v1.DefaultKeepAliveFrame)); got < 2 {
		t.Fatalf("keepalive comments during a %v upstream gap = %d, want >= 2\n%s", upstreamSilence, got, body)
	}
	mustContain(t, "cross-shape stream", body, "chat.completion.chunk")
	mustContain(t, "cross-shape stream", body, "Hello")
	assertWellFormedSSE(t, body)
}

// StreamKeepAlive = 0 leaves the stream exactly as it was.
func TestDispatch_KeepAlive_DisabledEmitsNothing(t *testing.T) {
	up := silentGapUpstream(t, anthropicStreamHead, anthropicStreamTail)
	cat, rk := keepAliveCatalog(t, up.URL)
	d := keepAliveDeps(t, cat, 0)

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r = withNormalContext(r, rk)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.Anthropic,
		Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ModelName: "test-model",
		Stream:    true,
	})

	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if got := strings.Count(body, "event: ping"); got != 0 {
		t.Fatalf("ping frames with the keepalive disabled = %d, want 0\n%s", got, body)
	}
	if strings.Contains(body, string(v1.DefaultKeepAliveFrame)) {
		t.Fatalf("keepalive comment emitted with the keepalive disabled\n%s", body)
	}
}
