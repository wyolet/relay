package otlpreceiver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	"github.com/wyolet/relay/sdk/client"
	"github.com/wyolet/relay/sdk/telemetry"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Upstream responses as each provider sends them, with the token breakdowns each adapter maps: cached, reasoning and audio tokens on the Chat Completions shape, cache reads and writes on the Messages shape, thoughts on generateContent.
const (
	chatCompletionsBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"acme-large","choices":[{"index":0,"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":1024,"audio_tokens":40},"completion_tokens_details":{"reasoning_tokens":128,"audio_tokens":16}}}`
	messagesBody        = `{"id":"msg_1","type":"message","role":"assistant","model":"acme-large","content":[{"type":"text","text":"Hello."}],"stop_reason":"end_turn","usage":{"input_tokens":90,"cache_read_input_tokens":2000,"cache_creation_input_tokens":500,"output_tokens":210}}`
	generateBody        = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":800,"cachedContentTokenCount":600,"candidatesTokenCount":150,"thoughtsTokenCount":64,"totalTokenCount":1014},"modelVersion":"acme-large","responseId":"resp-g1"}`
	// messagesStream carries the cache counts only on message_start, as the provider sends them.
	messagesStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"acme-large\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":90,\"cache_read_input_tokens\":2000,\"cache_creation_input_tokens\":500,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello.\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":210}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// serialized makes the receiver fixture safe to serve from an httptest server: the handler and the test's reads take turns.
type serialized struct {
	mu sync.Mutex
	fx *fixture
}

func (s *serialized) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fx.handler.ServeHTTP(w, r)
}

// TestSDKReportsMatchProxiedUsage is the acceptance test of reporting through the SDK: for the same upstream response, the usage event the receiver builds from the SDK's export carries the tokens and cost the proxied path stores, so nothing is counted twice or lost.
func TestSDKReportsMatchProxiedUsage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		translator v1.Translator
		body       string
		stream     bool
		build      func(baseURL string, opts ...client.Option) *client.Client
	}{
		{name: "chat completions: cached, reasoning, audio", translator: openai.CCTranslator{}, body: chatCompletionsBody, build: func(u string, o ...client.Option) *client.Client { return client.OpenAI(u, "sk-test", o...) }},
		{name: "messages: cache read and write", translator: anthropic.AnthropicTranslator{}, body: messagesBody, build: func(u string, o ...client.Option) *client.Client { return client.Anthropic(u, "sk-test", o...) }},
		{name: "messages stream: cache counts on message_start", translator: anthropic.AnthropicTranslator{}, body: messagesStream, stream: true, build: func(u string, o ...client.Option) *client.Client { return client.Anthropic(u, "sk-test", o...) }},
		{name: "generate content: thoughts", translator: gemini.GeminiTranslator{}, body: generateBody, build: func(u string, o ...client.Option) *client.Client { return client.Gemini(u, "sk-test", o...) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The proxied path: relay summarises the upstream body with the binding's translator and prices it at the binding's rate sheet.
			proxied, err := v1.ExtractSummary(tc.translator, []byte(tc.body))
			if err != nil || len(proxied.Tokens) == 0 {
				t.Fatalf("proxied summary: %v %v", proxied, err)
			}
			fx := newFixtureWith(t, fixtureOptions{reasoningRate: 3.5})
			fx.captureContent, fx.payloads.enabled = true, true
			wantCost, _, ok := fx.ownPrice.CostNanos(proxied.Tokens)
			if !ok {
				t.Fatal("proxied tokens are unpriced")
			}

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			receiver := &serialized{fx: fx}
			relay := httptest.NewServer(receiver)
			defer relay.Close()

			emitter := telemetry.New(telemetry.WithEndpoint(relay.URL+"/otlp"), telemetry.WithAPIKey(relayKey), telemetry.WithContent(true))
			defer emitter.Close(context.Background())
			c := tc.build(upstream.URL, client.WithTelemetry(emitter))
			req := &v1.Request{Model: v1.ModelRefs{"acme-large"}, Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "Say hello."}}}}}
			if tc.stream {
				s, err := c.GenerateStream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				for {
					if _, err := s.Recv(); err != nil {
						if !errors.Is(err, io.EOF) {
							t.Fatal(err)
						}
						break
					}
				}
				_ = s.Close()
			} else if _, err := c.Generate(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if err := emitter.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}

			receiver.mu.Lock()
			defer receiver.mu.Unlock()
			if len(fx.events) != 1 {
				t.Fatalf("recorded %d usage events, want 1", len(fx.events))
			}
			ev := fx.events[0]
			if !reflect.DeepEqual(ev.Tokens, proxied.Tokens) {
				t.Errorf("tokens = %v, proxied path stores %v", ev.Tokens, proxied.Tokens)
			}
			if ev.CostNanos == nil || *ev.CostNanos != wantCost {
				t.Errorf("cost = %v, proxied path charges %d", ev.CostNanos, wantCost)
			}
			if ev.FinishReason != string(proxied.FinishReason) || ev.Streamed != tc.stream || ev.Status != http.StatusOK || ev.ErrorKind != "" {
				t.Errorf("finish %q streamed %v status %d error %q", ev.FinishReason, ev.Streamed, ev.Status, ev.ErrorKind)
			}
			// The content event collapses into the same call and its content is stored.
			if len(fx.payloads.records) != 1 || fx.payloads.records[0].RequestID != ev.RequestID {
				t.Fatalf("payload records = %d, want one for %s", len(fx.payloads.records), ev.RequestID)
			}
			var request, response map[string]json.RawMessage
			if json.Unmarshal(fx.payloads.records[0].RequestBody, &request) != nil || json.Unmarshal(fx.payloads.records[0].ResponseBody, &response) != nil {
				t.Fatalf("stored bodies are not JSON objects: %s / %s", fx.payloads.records[0].RequestBody, fx.payloads.records[0].ResponseBody)
			}
			if !strings.Contains(string(request["gen_ai.input.messages"]), "Say hello.") || !strings.Contains(string(response["gen_ai.output.messages"]), "Hello.") {
				t.Errorf("stored content: %s / %s", fx.payloads.records[0].RequestBody, fx.payloads.records[0].ResponseBody)
			}
		})
	}
}
