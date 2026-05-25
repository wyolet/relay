package adapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	pkggemini "github.com/wyolet/relay/pkg/adapters/gemini"
	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

const testGeminiModel = "gemini-1.5-pro"
const testGeminiKey = "test-key-abc"

// syncGeminiResponse is a realistic generateContent response body.
var syncGeminiResponse = `{
	"candidates":[{
		"content":{"role":"model","parts":[
			{"text":"Hello from Gemini!"},
			{"functionCall":{"name":"lookup","args":{"q":"relay"}}}
		]},
		"finishReason":"STOP",
		"index":0
	}],
	"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":8},
	"modelVersion":"gemini-1.5-pro"
}`

// streamGeminiFrames is a realistic streamGenerateContent SSE response.
var streamGeminiFrames = strings.Join([]string{
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello "}]},"index":0}],"modelVersion":"gemini-1.5-pro"}`,
	`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"relay"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":8}}`,
	``,
}, "\n\n")

// buildGeminiSpec replicates the Spec registration from cmd/relay/main.go exactly.
func buildGeminiSpec() *adapter.Spec {
	return (&adapter.Spec{
		Name: adapters.Gemini,
		Auth: adapter.AuthStrategy{Header: "x-goog-api-key"},
		UpstreamPathFn: func(model string, stream bool) string {
			if stream {
				return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
			}
			return "/v1beta/models/" + model + ":generateContent"
		},
		Translator:    pkggemini.GeminiTranslator{},
		ExtractTokens: pkggemini.ExtractTokens,
	}).Build()
}

// minimalGeminiBody returns a minimal Gemini generateContent request body.
func minimalGeminiBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGeminiIntegration_Sync_PathAuthAndCanonicalShape(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		// Assert sync path encodes the model correctly.
		wantPath := "/v1beta/models/" + testGeminiModel + ":generateContent"
		if r.URL.Path != wantPath {
			t.Errorf("sync path: got %q, want %q", r.URL.Path, wantPath)
		}
		// No ?alt=sse on sync.
		if r.URL.RawQuery != "" {
			t.Errorf("sync query: got %q, want empty", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, syncGeminiResponse)
	}))
	defer srv.Close()

	spec := buildGeminiSpec()
	resp, err := spec.PipelineAdapter().Call(
		context.Background(),
		srv.URL,
		testGeminiKey,
		minimalGeminiBody(t),
		nil,
		testGeminiModel,
		false, // sync
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if gotKey != testGeminiKey {
		t.Errorf("x-goog-api-key: got %q, want %q", gotKey, testGeminiKey)
	}
	_ = gotPath // already asserted inside handler

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// ParseResponse → verify canonical shape.
	canonical, err := pkggemini.GeminiTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(canonical.Output) != 2 {
		t.Fatalf("output items: got %d, want 2", len(canonical.Output))
	}
	msg, ok := canonical.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] type: %T", canonical.Output[0])
	}
	if len(msg.Content) == 0 {
		t.Fatal("message has no content")
	}
	otp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] type: %T", msg.Content[0])
	}
	if otp.Text != "Hello from Gemini!" {
		t.Errorf("text: %q", otp.Text)
	}
	fc, ok := canonical.Output[1].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("output[1] type: %T", canonical.Output[1])
	}
	if fc.Name != "lookup" {
		t.Errorf("function name: %q", fc.Name)
	}
	if canonical.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finish_reason: %s", canonical.FinishReason)
	}

	// ExtractTokens via the spec adapter.
	tokens := pkggemini.ExtractTokens(body)
	if tokens["input"] != 10 {
		t.Errorf("input tokens: %d", tokens["input"])
	}
	if tokens["output"] != 8 {
		t.Errorf("output tokens: %d", tokens["output"])
	}
}

func TestGeminiIntegration_Stream_PathAuthAndCanonicalShape(t *testing.T) {
	var gotPath, gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotQuery = r.URL.RawQuery

		wantPath := "/v1beta/models/" + testGeminiModel + ":streamGenerateContent"
		if r.URL.Path != wantPath {
			t.Errorf("stream path: got %q, want %q", r.URL.Path, wantPath)
		}
		if r.URL.RawQuery != "alt=sse" {
			t.Errorf("stream query: got %q, want %q", r.URL.RawQuery, "alt=sse")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streamGeminiFrames)
	}))
	defer srv.Close()

	spec := buildGeminiSpec()
	resp, err := spec.PipelineAdapter().Call(
		context.Background(),
		srv.URL,
		testGeminiKey,
		minimalGeminiBody(t),
		nil,
		testGeminiModel,
		true, // stream
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if gotKey != testGeminiKey {
		t.Errorf("x-goog-api-key: got %q, want %q", gotKey, testGeminiKey)
	}
	_ = gotPath
	_ = gotQuery // asserted inside handler

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Run each SSE frame through the stream translator and collect canonical output.
	translate := pkggemini.GeminiTranslator{}.NewToCanonicalStream()
	var allCanon strings.Builder
	for _, frame := range strings.Split(string(raw), "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		out, err := translate([]byte(frame + "\n\n"))
		if err != nil {
			t.Fatalf("translate frame: %v", err)
		}
		allCanon.Write(out)
	}

	canon := allCanon.String()
	if !strings.Contains(canon, v1.EventGenerationCreated) {
		t.Error("missing generation.created")
	}
	if !strings.Contains(canon, v1.EventItemStarted) {
		t.Error("missing item.started")
	}
	if !strings.Contains(canon, v1.EventItemDelta) {
		t.Error("missing item.delta")
	}
	if !strings.Contains(canon, v1.EventItemCompleted) {
		t.Error("missing item.completed")
	}
	if !strings.Contains(canon, v1.EventGenerationCompleted) {
		t.Error("missing generation.completed")
	}
	if !strings.Contains(canon, "lookup") {
		t.Error("function name 'lookup' missing from canonical stream")
	}
	if !strings.Contains(canon, `"input"`) {
		t.Error("usage input missing from canonical stream")
	}
	// generation.completed should report tool_calls finish reason because a
	// functionCall part appeared.
	if !strings.Contains(canon, string(v1.FinishReasonToolCalls)) {
		t.Errorf("finish_reason tool_calls missing from canonical stream:\n%s", canon)
	}
}
