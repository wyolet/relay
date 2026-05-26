package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

func sampleReq() *v1.Request {
	return &v1.Request{
		Model:        v1.ModelRefs{"some-model"},
		Instructions: "be concise",
		CacheConfig:  &v1.CacheConfig{Instructions: true},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
}

// --- Relay target (identity translator) ---

func TestRelay_SerializesCanonicalAndDecodesResponse(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		resp := &v1.Response{
			ID: "resp_1", Object: "response", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop,
			Output: []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hello"}}}},
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	resp, err := Relay(srv.URL, "rk-secret").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/generate" {
		t.Errorf("path: %q", gotPath)
	}
	if gotAuth != "Bearer rk-secret" {
		t.Errorf("auth: %q", gotAuth)
	}
	var wire map[string]any
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	if _, ok := wire["model"].(string); !ok {
		t.Errorf("model should be a string, got %T", wire["model"])
	}
	if wire["output_mode"] != "sync" {
		t.Errorf("output_mode: %v", wire["output_mode"])
	}
	if cc, ok := wire["cache_config"].(map[string]any); !ok || cc["instructions"] != true {
		t.Errorf("cache_config not serialized: %v", wire["cache_config"])
	}
	if resp.ID != "resp_1" || len(resp.Output) != 1 {
		t.Errorf("response: %+v", resp)
	}
}

func TestRelay_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range []v1.SSEFrame{
			{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"resp_1"}`)},
			{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"msg_1","kind":"output_text"}`)},
			{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"resp_1","status":"completed","finish_reason":"stop"}`)},
		} {
			_, _ = w.Write(f.Bytes())
		}
	}))
	defer srv.Close()

	got := drain(t, Relay(srv.URL, "rk"))
	want := []string{v1.EventGenerationCreated, v1.EventItemDelta, v1.EventGenerationCompleted}
	assertEvents(t, got, want)
}

func TestRelay_Stream_ReasoningSpan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		write := func(f v1.SSEFrame) {
			_, _ = w.Write(f.Bytes())
			if fl != nil {
				fl.Flush()
			}
		}
		write(v1.SSEFrame{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"r1"}`)})
		write(v1.SSEFrame{Event: v1.EventItemStarted, Data: []byte(`{"item_id":"rs1","item_type":"reasoning","index":0}`)})
		time.Sleep(2 * time.Millisecond)
		write(v1.SSEFrame{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"rs1","kind":"reasoning","delta":"x"}`)})
		write(v1.SSEFrame{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"m1","kind":"text","delta":"hi"}`)})
		write(v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"r1","status":"completed"}`)})
	}))
	defer srv.Close()

	stream, err := Relay(srv.URL, "rk").GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("recv: %v", err)
		}
	}

	tm := stream.Timing()
	// Upstream timing populated in the relay-identical shape: Start collapsed
	// onto ResponseStart (TTFT), ResponseEnd at stream close.
	if tm.Upstream.ResponseStart <= 0 {
		t.Fatalf("TTFT not stamped: %+v", tm.Upstream)
	}
	if tm.Upstream.Start != tm.Upstream.ResponseStart {
		t.Fatalf("client Start should equal ResponseStart (no relay leg): %+v", tm.Upstream)
	}
	if tm.Upstream.ResponseEnd < tm.Upstream.ResponseStart {
		t.Fatalf("ResponseEnd %d before ResponseStart %d", tm.Upstream.ResponseEnd, tm.Upstream.ResponseStart)
	}
	if tm.Reasoning == nil {
		t.Fatal("reasoning span not detected")
	}
	if tm.Reasoning.Start <= 0 || tm.Reasoning.End < tm.Reasoning.Start {
		t.Fatalf("bad reasoning span: %+v", tm.Reasoning)
	}
}

func TestRelay_Stream_NoReasoning_NoSpan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, f := range []v1.SSEFrame{
			{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"r1"}`)},
			{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"m1","kind":"text","delta":"hi"}`)},
			{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"r1","status":"completed"}`)},
		} {
			_, _ = w.Write(f.Bytes())
		}
	}))
	defer srv.Close()

	stream, err := Relay(srv.URL, "rk").GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("recv: %v", err)
		}
	}
	tm := stream.Timing()
	if tm.Reasoning != nil {
		t.Fatalf("expected no reasoning span, got %+v", tm.Reasoning)
	}
	if tm.Upstream.ResponseStart <= 0 || tm.Upstream.ResponseEnd < tm.Upstream.ResponseStart {
		t.Fatalf("upstream timing should still be populated: %+v", tm.Upstream)
	}
}

func TestRelay_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"translate_request","message":"input is required"}}`))
	}))
	defer srv.Close()

	_, err := Relay(srv.URL, "rk").Generate(context.Background(), sampleReq())
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 || apiErr.Code != "translate_request" {
		t.Errorf("apiErr: %+v", apiErr)
	}
}

// --- OpenAI direct target (CC translator) — bypasses relay ---

func TestOpenAIDirect_TranslatesToCCWireAndBack(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path: %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth: %q", r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"cc1","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer srv.Close()

	resp, err := OpenAI(srv.URL, "sk-test").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	// Canonical request was translated to CC wire (messages array, not "input").
	if _, ok := gotBody["messages"].([]any); !ok {
		t.Errorf("expected CC 'messages' array, got keys %v", keysOf(gotBody))
	}
	// CC response parsed back to canonical.
	if resp.FinishReason != v1.FinishReasonStop || len(resp.Output) == 0 {
		t.Errorf("canonical response: %+v", resp)
	}
}

func TestOpenAIDirect_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal CC stream: one content delta, then [DONE].
		chunks := []string{
			`data: {"id":"cc1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n\n"))
		}
	}))
	defer srv.Close()

	got := drain(t, OpenAI(srv.URL, "sk"))
	// CC stream → canonical events: at minimum a generation.completed terminates it.
	if !contains(got, v1.EventGenerationCompleted) {
		t.Errorf("expected %q among canonical events, got %v", v1.EventGenerationCompleted, got)
	}
}

// --- Anthropic direct target ---

func TestAnthropicDirect_HeadersAndWire(t *testing.T) {
	var gotBody map[string]any
	var gotVer, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %q", r.URL.Path)
		}
		gotKey = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"msg1","type":"message","role":"assistant",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer srv.Close()

	resp, err := Anthropic(srv.URL, "ak-test").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "ak-test" {
		t.Errorf("x-api-key: %q", gotKey)
	}
	if gotVer != "2023-06-01" {
		t.Errorf("anthropic-version: %q", gotVer)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Errorf("expected Anthropic 'max_tokens' in wire, got %v", keysOf(gotBody))
	}
	if len(resp.Output) == 0 {
		t.Errorf("canonical response empty: %+v", resp)
	}
}

// --- live tests (skipped unless env configured) ---

// TestLive_OpenAI hits the real OpenAI API. Set OPENAI_API_KEY (and optionally
// OPENAI_MODEL, default gpt-4o-mini) to run.
func TestLive_OpenAI(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("set OPENAI_API_KEY to run the live OpenAI test")
	}
	model := envOr("OPENAI_MODEL", "gpt-4o-mini")
	c := OpenAI("https://api.openai.com", key, WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	resp, err := c.Generate(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	if outputText(resp) == "" {
		t.Errorf("empty output: %+v", resp)
	}
	t.Logf("OpenAI %s replied: %q", model, outputText(resp))
}

// TestLive_Ollama hits a real Ollama (OpenAI-compatible) endpoint. Set
// OLLAMA_BASE_URL (e.g. http://localhost:11434) and OLLAMA_MODEL to run.
func TestLive_Ollama(t *testing.T) {
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		t.Skip("set OLLAMA_BASE_URL (and OLLAMA_MODEL) to run the live Ollama test")
	}
	model := envOr("OLLAMA_MODEL", "llama3.2")
	c := OpenAI(base, "ollama", WithHTTPClient(&http.Client{Timeout: 60 * time.Second}))
	resp, err := c.Generate(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	if outputText(resp) == "" {
		t.Errorf("empty output: %+v", resp)
	}
	t.Logf("Ollama %s replied: %q", model, outputText(resp))

	// Streaming through the same bridge (CC SSE → canonical events).
	stream, err := c.GenerateStream(context.Background(), liveReq(model))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var events, deltaText strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events.WriteString(ev.Type + " ")
		if ev.Type == v1.EventItemDelta {
			var d struct {
				Delta string `json:"delta"`
				Text  string `json:"text"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			deltaText.WriteString(d.Delta + d.Text)
		}
	}
	if !strings.Contains(events.String(), v1.EventGenerationCompleted) {
		t.Errorf("stream missing generation.completed; events: %s", events.String())
	}
	t.Logf("Ollama %s streamed events: [%s] text=%q", model, strings.TrimSpace(events.String()), deltaText.String())
}

// --- env-var defaults ---

func TestRelay_EnvDefaults(t *testing.T) {
	var gotAuth, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer srv.Close()

	t.Setenv(EnvBaseURL, srv.URL)
	t.Setenv(EnvAPIKey, "rk-from-env")

	if _, err := Relay("", "").Generate(context.Background(), sampleReq()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer rk-from-env" {
		t.Errorf("auth: %q", gotAuth)
	}
	if gotHost != strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("host: %q want %q", gotHost, srv.URL)
	}
}

func TestRelay_ExplicitArgsBeatEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://env-host.invalid")
	t.Setenv(EnvAPIKey, "rk-from-env")

	c := Relay("http://explicit.invalid", "rk-explicit")
	if c.baseURL != "http://explicit.invalid" || c.apiKey != "rk-explicit" {
		t.Errorf("explicit args overridden by env: base=%q key=%q", c.baseURL, c.apiKey)
	}
}

func TestRelay_MissingConfig_FailsOnCallNotConstruction(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAPIKey, "")

	c := Relay("", "") // must not panic / must return a client
	if c == nil {
		t.Fatal("Relay returned nil")
	}
	_, err := c.Generate(context.Background(), sampleReq())
	if err == nil || !strings.Contains(err.Error(), "missing config") {
		t.Fatalf("want missing-config error on call, got %v", err)
	}

	_, serr := c.GenerateStream(context.Background(), sampleReq())
	if serr == nil || !strings.Contains(serr.Error(), "missing config") {
		t.Fatalf("want missing-config error on stream call, got %v", serr)
	}
}

func TestVendorClients_IgnoreRelayEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://relay-env.invalid")
	t.Setenv(EnvAPIKey, "rk-from-env")

	c := OpenAI("https://api.openai.com", "")
	if c.configErr != nil {
		t.Errorf("vendor client should never carry relay config error: %v", c.configErr)
	}
	if c.baseURL != "https://api.openai.com" {
		t.Errorf("vendor baseURL pulled from relay env: %q", c.baseURL)
	}
}

func TestRelay_UsageAndHeaderEnv(t *testing.T) {
	var gotUsage, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUsage = r.Header.Get("X-WR-Usage")
		gotCustom = r.Header.Get("X-Custom")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer srv.Close()

	t.Setenv(EnvBaseURL, srv.URL)
	t.Setenv(EnvAPIKey, "rk")
	t.Setenv(EnvUsage, "full")
	t.Setenv(EnvHeaders, "X-Custom=hi, X-Other=2")

	if _, err := Relay("", "").Generate(context.Background(), sampleReq()); err != nil {
		t.Fatal(err)
	}
	if gotUsage != "full" {
		t.Errorf("X-WR-Usage: %q", gotUsage)
	}
	if gotCustom != "hi" {
		t.Errorf("X-Custom: %q", gotCustom)
	}
}

func TestRelay_ExplicitHeaderBeatsEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://x.invalid")
	t.Setenv(EnvAPIKey, "rk")
	t.Setenv(EnvUsage, "full")

	c := Relay("", "", WithHeader(headerUsage, "off"))
	if c.headers[headerUsage] != "off" {
		t.Errorf("explicit WithHeader lost to env: %q", c.headers[headerUsage])
	}
}

func TestRelay_BadHeaderEnv_FailsOnCall(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://x.invalid")
	t.Setenv(EnvAPIKey, "rk")
	t.Setenv(EnvHeaders, "no-equals-sign")

	_, err := Relay("", "").Generate(context.Background(), sampleReq())
	if err == nil || !strings.Contains(err.Error(), "WR_HEADERS") {
		t.Fatalf("want WR_HEADERS parse error on call, got %v", err)
	}
}

func TestRelay_TimeoutEnv_AppliesToSyncNotStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer srv.Close()

	t.Setenv(EnvBaseURL, srv.URL)
	t.Setenv(EnvAPIKey, "rk")
	t.Setenv(EnvTimeout, "20ms")

	_, err := Relay("", "").Generate(context.Background(), sampleReq())
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline-exceeded on sync, got %v", err)
	}

	if c := Relay("", ""); c.syncTimeout != 20*time.Millisecond {
		t.Errorf("syncTimeout: %v", c.syncTimeout)
	}
}

func TestRelay_BadTimeoutEnv_FailsOnCall(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://x.invalid")
	t.Setenv(EnvAPIKey, "rk")
	t.Setenv(EnvTimeout, "not-a-duration")

	_, err := Relay("", "").Generate(context.Background(), sampleReq())
	if err == nil || !strings.Contains(err.Error(), "WR_TIMEOUT") {
		t.Fatalf("want WR_TIMEOUT parse error on call, got %v", err)
	}
}

// --- helpers ---

func liveReq(model string) *v1.Request {
	mt := 64
	return &v1.Request{
		Model: v1.ModelRefs{model},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "Reply with the single word: pong"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			model: {Sampling: &v1.SamplingParams{MaxTokens: &mt}},
		},
	}
}

func outputText(resp *v1.Response) string {
	var b strings.Builder
	for _, it := range resp.Output {
		if m, ok := it.(*v1.Message); ok {
			for _, p := range m.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					b.WriteString(tp.Text)
				case *v1.TextPart:
					b.WriteString(tp.Text)
				}
			}
		}
	}
	return b.String()
}

func drain(t *testing.T, c *Client) []string {
	t.Helper()
	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var got []string
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev.Type)
	}
	return got
}

func assertEvents(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
