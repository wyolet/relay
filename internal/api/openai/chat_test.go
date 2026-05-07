package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/routing"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
)

// --- ChatCompletions handler tests ---

// fakeResolver returns a *routing.Resolver backed by an in-memory catalog
// containing two models: "gpt-4" (provider openai) and "mymodel" (provider
// ollama, upstream "upstream-model").
func fakeResolver() *routing.Resolver {
	store := catalog.NewMemStore(
		&catalog.Provider{
			Metadata: catalog.Metadata{Name: "openai"},
			Spec:     catalog.ProviderSpec{Kind: catalog.PKOpenAI},
		},
		&catalog.Provider{
			Metadata: catalog.Metadata{Name: "ollama"},
			Spec:     catalog.ProviderSpec{Kind: catalog.PKOllama},
		},
		&catalog.Model{
			Metadata: catalog.Metadata{Name: "gpt-4"},
			Spec:     catalog.ModelSpec{Provider: "openai", UpstreamName: "gpt-4"},
		},
		&catalog.Model{
			Metadata: catalog.Metadata{Name: "mymodel"},
			Spec:     catalog.ModelSpec{Provider: "ollama", UpstreamName: "upstream-model"},
		},
	)
	return routing.New(store)
}

func TestChatCompletions_HappyPath(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{
				"X-Relay-Status": "200",
				"Content-Type":   "application/json",
			},
		}
		ch.Out <- &transport.Message{Body: []byte(`{"hello":"world"}`)}
		ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestChatCompletions_StreamingPath(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "text/event-stream"},
		}
		ch.Out <- &transport.Message{Body: []byte("data: chunk1\n")}
		ch.Out <- &transport.Message{Body: []byte("data: chunk2\n")}
		ch.Out <- &transport.Message{Body: []byte("data: chunk3\n")}
		ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	want := "data: chunk1\ndata: chunk2\ndata: chunk3\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestChatCompletions_ModelNotFound(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error { return nil }

	body := `{"model":"unknown-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var env errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", env.Error.Code)
	}
}

func TestChatCompletions_BadJSON(t *testing.T) {
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error { return nil }

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestChatCompletions_StreamingMidstreamError(t *testing.T) {
	errEnvJSON := `{"error":{"message":"upstream lost","type":"upstream_error","code":"upstream_unavailable"}}`
	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "text/event-stream"},
		}
		ch.Out <- &transport.Message{Body: []byte("data: chunk1\n\n")}
		ch.Out <- &transport.Message{
			Headers: map[string]string{
				"X-Relay-Status": "502",
				"X-Relay-Final":  "true",
			},
			Body: []byte(errEnvJSON),
		}
		return nil
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 (already committed)", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "data: chunk1") {
		t.Errorf("body missing original chunk: %q", got)
	}
	if !strings.Contains(got, "data: "+errEnvJSON) {
		t.Errorf("body missing SSE error event: %q", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("body missing [DONE]: %q", got)
	}
}

func TestChatCompletions_StreamingClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pipeline sends first message then blocks until context is done.
	runPipeline := func(pCtx context.Context, ch *transport.Channel, plan *RequestPlan) error {
		defer close(ch.Out)
		ch.Out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "text/event-stream"},
		}
		ch.Out <- &transport.Message{Body: []byte("data: chunk1\n\n")}
		// Wait for context cancellation.
		<-pCtx.Done()
		return pCtx.Err()
	}

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Cancel after a short time to unblock the handler.
	go func() {
		cancel()
	}()

	ChatCompletions(fakeResolver(), runPipeline)(rec, req)

	got := rec.Body.String()
	// No SSE error event should be emitted on client cancel.
	if strings.Contains(got, "data: {") && strings.Contains(got, "error") {
		t.Errorf("error event emitted after client cancel: %q", got)
	}
}

func TestChatCompletions_AttributionFlowsFromContext(t *testing.T) {
	var capturedAttribution map[string]string
	innerHandler := ChatCompletions(fakeResolver(), func(_ context.Context, ch *transport.Channel, _ *RequestPlan) error {
		defer close(ch.Out)
		msg := <-ch.In
		capturedAttribution = msg.Attribution
		ch.Out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"},
		}
		ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	})

	// Wrap with reqid.Middleware so attribution is parsed from X-Relay-Metadata.
	wrapped := reqid.Middleware(slog.Default())(innerHandler)

	body := `{"model":"gpt-4","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Relay-Metadata", "env=test")
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if capturedAttribution == nil || capturedAttribution["env"] != "test" {
		t.Errorf("Attribution not threaded: %v", capturedAttribution)
	}
}

func TestChatCompletions_BodyAttributionRichMode(t *testing.T) {
	withRich(true, func() {
		var capturedAttribution map[string]string
		h := ChatCompletions(fakeResolver(), func(_ context.Context, ch *transport.Channel, _ *RequestPlan) error {
			defer close(ch.Out)
			msg := <-ch.In
			capturedAttribution = msg.Attribution
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return nil
		})

		body := `{"model":"gpt-4","metadata":{"caller":"sdk"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if capturedAttribution == nil || capturedAttribution["caller"] != "sdk" {
			t.Errorf("body attribution not threaded: %v", capturedAttribution)
		}
	})
}

func TestChatCompletions_HeaderWinsOverBody(t *testing.T) {
	withRich(true, func() {
		var capturedAttribution map[string]string
		innerHandler := ChatCompletions(fakeResolver(), func(_ context.Context, ch *transport.Channel, _ *RequestPlan) error {
			defer close(ch.Out)
			msg := <-ch.In
			capturedAttribution = msg.Attribution
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return nil
		})

		wrapped := reqid.Middleware(slog.Default())(innerHandler)
		body := `{"model":"gpt-4","metadata":{"caller":"body"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-Relay-Metadata", "caller=header")
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)

		if capturedAttribution == nil || capturedAttribution["caller"] != "header" {
			t.Errorf("header should win over body: %v", capturedAttribution)
		}
	})
}

func TestChatCompletions_MinimalModeBodyAttributionIgnored(t *testing.T) {
	withRich(false, func() {
		var capturedAttribution map[string]string
		h := ChatCompletions(fakeResolver(), func(_ context.Context, ch *transport.Channel, _ *RequestPlan) error {
			defer close(ch.Out)
			msg := <-ch.In
			capturedAttribution = msg.Attribution
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return nil
		})

		body := `{"model":"gpt-4","metadata":{"caller":"sdk"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if capturedAttribution != nil {
			t.Errorf("minimal mode: body attribution should be nil, got %v", capturedAttribution)
		}
	})
}

func TestChatCompletions_RawBodyForwarded(t *testing.T) {
	withRich(true, func() {
		var capturedBody []byte
		h := ChatCompletions(fakeResolver(), func(_ context.Context, ch *transport.Channel, _ *RequestPlan) error {
			defer close(ch.Out)
			msg := <-ch.In
			capturedBody = msg.Body
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
			ch.Out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return nil
		})

		// model name matches upstream, so Raw is forwarded as-is.
		body := `{"model":"gpt-4","max_tokens":42,"messages":[]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if string(capturedBody) != body {
			t.Errorf("forwarded body = %s, want %s", capturedBody, body)
		}
	})
}
