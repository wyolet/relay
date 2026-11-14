package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/sdk/adapters/openai"
)

const (
	ccContentChunk = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}]}\n\n"
	ccUsageChunk   = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n"
	ccDone         = "data: [DONE]\n\n"
)

// usageOptInUpstream answers like a Chat Completions upstream: the usage chunk
// is sent only when the request asked for it. It records the request body.
func usageOptInUpstream(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(ccContentChunk))
		if strings.Contains(string(b), `"include_usage":true`) {
			_, _ = w.Write([]byte(ccUsageChunk))
		}
		_, _ = w.Write([]byte(ccDone))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return got }
}

// Same-shape streamed Chat Completions: relay asks the upstream for usage so token accounting sees it, and keeps the extra usage chunk from a caller that did not ask for it.
func TestBytePassStreamUsageOptIn(t *testing.T) {
	const plain = `{"model":"test-model","stream":true,"messages":[]}`
	const asked = `{"model":"test-model","stream":true,"stream_options":{"include_usage":true},"messages":[]}`
	for name, tc := range map[string]struct {
		requestUsage bool
		body         string
		wantUpstream string
		wantCaller   string
	}{
		"relay asks and hides the usage chunk":       {true, plain, `{"model":"test-model","stream":true,"messages":[],"stream_options":{"include_usage":true}}`, ccContentChunk + ccDone},
		"the caller's own ask is forwarded as it is": {true, asked, asked, ccContentChunk + ccUsageChunk + ccDone},
		"opt-in off forwards the request unchanged":  {false, plain, plain, ccContentChunk + ccDone},
		"opt-in off keeps the caller's ask":          {false, asked, asked, ccContentChunk + ccUsageChunk + ccDone},
	} {
		t.Run(name, func(t *testing.T) {
			up, received := usageOptInUpstream(t)
			cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
			pointHostAt(t, cat, up.URL)
			d := buildRunnableDeps(t, cat)
			useSpecs(&d, adapter.NewRegistry((&adapter.Spec{
				Name:          adapters.OpenAI,
				DefaultPath:   "/v1/chat/completions",
				Auth:          adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
				Translator:    openai.CCTranslator{},
				ExtractTokens: openai.ExtractTokens,
				StreamUsage:   &adapter.StreamUsageOptIn{Request: openai.RequestStreamUsage, IsUsageFrame: openai.IsUsageOnlyChunk},
			}).Build()))
			d.RequestStreamUsage = tc.requestUsage

			w := dispatch(d, rk, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(tc.body), ModelName: "test-model", Stream: true})

			if w.Code != http.StatusOK || w.Body.String() != tc.wantCaller {
				t.Errorf("caller got %d %q, want %q", w.Code, w.Body, tc.wantCaller)
			}
			if got := received(); got != tc.wantUpstream {
				t.Errorf("upstream request = %s, want %s", got, tc.wantUpstream)
			}
		})
	}
}
