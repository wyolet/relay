package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/adapters"
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
		Translator:  productionSpec(adapters.Anthropic).Translator,
	}).Build()
	cc := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  productionSpec(adapters.OpenAI).Translator,
	}).Build()
	canonical := (&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build()
	return adapter.NewRegistry(anth, cc, canonical)
}

func keepAliveCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	pointHostAt(t, cat, upstreamURL)
	return cat, rk
}

func keepAliveDeps(t *testing.T, cat *catalog.Catalog, every time.Duration) Deps {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, keepAliveRegistry())
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

// While the upstream is silent mid-stream relay keeps the connection alive with a frame of the inbound shape: an Anthropic caller gets `ping`, everyone else an SSE comment. The frames land only between the stream's own, so taking them out leaves exactly the stream a relay without keepalive sends.
func TestDispatch_KeepAlive(t *testing.T) {
	run := func(t *testing.T, inbound adapters.Name, every time.Duration) string {
		t.Helper()
		up := silentGapUpstream(t, anthropicStreamHead, anthropicStreamTail)
		cat, rk := keepAliveCatalog(t, up.URL)
		w := dispatch(keepAliveDeps(t, cat, every), rk, DispatchInput{
			Inbound:   inbound,
			Body:      []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			ModelName: "test-model",
			Stream:    true,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		return string(clockFields.ReplaceAll(w.Body.Bytes(), []byte(`"$1":0`)))
	}
	for name, tc := range map[string]struct {
		inbound adapters.Name
		frame   string
	}{
		"byte-pass, the shape's own ping": {adapters.Anthropic, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		"cross-shape, an SSE comment":     {adapters.OpenAI, ": keepalive\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			quiet := run(t, tc.inbound, 0)
			if strings.Contains(quiet, tc.frame) {
				t.Fatalf("keepalive frame emitted with the keepalive disabled:\n%s", quiet)
			}
			if tc.inbound == adapters.Anthropic && quiet != anthropicStreamHead+anthropicStreamTail {
				t.Errorf("byte-pass stream = %q, want the upstream's bytes", quiet)
			}
			kept := run(t, tc.inbound, keepAliveInterval)
			if n := strings.Count(kept, tc.frame); n < 2 {
				t.Errorf("keepalive frames during a %v upstream gap = %d, want >= 2\n%s", upstreamSilence, n, kept)
			}
			if got := strings.ReplaceAll(kept, tc.frame, ""); got != quiet {
				t.Errorf("stream without its keepalive frames\n got %q\nwant %q", got, quiet)
			}
		})
	}
}
