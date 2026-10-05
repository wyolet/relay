package inference

// Same-shape streamed Chat Completions: relay asks the upstream for usage so
// token accounting sees it, and keeps the extra usage chunk from a caller
// that did not ask for it.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/host"
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

func streamUsageDispatch(t *testing.T, requestUsage bool, body string) (upstreamBody, callerBody string) {
	t.Helper()
	up, received := usageOptInUpstream(t)
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	h := *cat.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: up.URL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	d := buildRunnableDeps(t, cat)
	reg := adapter.NewRegistry((&adapter.Spec{
		Name:          adapters.OpenAI,
		DefaultPath:   "/v1/chat/completions",
		Auth:          adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:    openai.CCTranslator{},
		ExtractTokens: openai.ExtractTokens,
		StreamUsage:   &adapter.StreamUsageOptIn{Request: openai.RequestStreamUsage, IsUsageFrame: openai.IsUsageOnlyChunk},
	}).Build())
	d.Specs = reg
	d.Adapters = reg.AdapterMap()
	d.RequestStreamUsage = requestUsage

	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil), rk)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(body), ModelName: "test-model", Stream: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return received(), w.Body.String()
}

func TestBytePassStreamRequestsUsageAndHidesIt(t *testing.T) {
	upstream, caller := streamUsageDispatch(t, true, `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	mustContain(t, "upstream request", upstream, `"include_usage":true`)
	if caller != ccContentChunk+ccDone {
		t.Fatalf("caller stream = %q, want the upstream stream without the usage chunk", caller)
	}
}

func TestBytePassStreamKeepsUsageTheCallerAskedFor(t *testing.T) {
	_, caller := streamUsageDispatch(t, true, `{"model":"test-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if caller != ccContentChunk+ccUsageChunk+ccDone {
		t.Fatalf("caller stream = %q, want the upstream stream verbatim", caller)
	}
}

func TestBytePassStreamUsageOffForwardsRequestUnchanged(t *testing.T) {
	upstream, caller := streamUsageDispatch(t, false, `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	mustNotContain(t, "upstream request", upstream, "stream_options")
	if caller != ccContentChunk+ccDone {
		t.Fatalf("caller stream = %q", caller)
	}
}
