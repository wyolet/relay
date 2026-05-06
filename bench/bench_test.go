// Package bench measures Relay's internal per-request overhead in isolation.
//
// # Assumptions and scope
//
// The upstream provider is an in-process httptest.Server returning a canned
// 200 chat-completions response (~500 bytes). This isolates Relay's own
// contribution — auth middleware, allowlist header scrubbing, request-ID
// injection, body-limit enforcement, huma/humachi routing, model resolution,
// key selection, and pipeline orchestration — from real network latency, real
// datastores, and multi-pod coordination.
//
// The bench exercises the full huma + humachi production hot path: chat
// completions are registered via huma.Register with a RawBody input struct,
// and the body-restore pattern from delegateBody (PER-256) is applied so the
// downstream handler can re-read r.Body after huma consumes it. This matches
// cmd/relay/openapi.go exactly.
//
// What IS measured (the realistic post-PER-249/PER-251 hot path):
//   - Bearer-token auth middleware (auth.Middleware)
//   - Inbound-header allowlist (httpheader.StripInbound, inside ChatCompletions)
//   - Request-ID middleware (reqid.Middleware)
//   - Body-limit middleware (httpmw.LimitBody)
//   - Huma/humachi routing + body-passthrough layer (full production path)
//   - Model resolution from in-memory configstore
//   - Key selection via keypool.Selector (in-memory state, no Redis round-trip)
//   - Rate-limit Reserve+Commit via pkg/limit (in-memory sliding window)
//   - Pipeline.Run orchestration
//   - Provider outbound call to in-process stub (excluded via baseline subtract)
//
// What is NOT measured:
//   - Real Redis / Postgres / ClickHouse latency
//   - Network round-trip to a remote upstream
//   - Multi-pod coordination or cache-warming
//
// # Chat-overhead parameterization (PER-260 / M7)
//
// BenchmarkRelayChatOverhead_Rich exercises the M7 default: RELAY_RICH_PARSING=on
// (full body parse via Parse — model, stream, user, metadata validation, messages
// extraction). BenchmarkRelayChatOverhead_Minimal exercises the legacy partial-parse
// path: RELAY_RICH_PARSING=off (model/stream/user/raw only; metadata and messages left
// nil). Both assert the same non-streaming SLO (p99 ≤ 5ms, p50 ≤ 1ms). The delta
// between modes is the observable cost of the M7 full-parse default. Each bench
// calls b.Cleanup to restore SetRichParsing to its prior value so test ordering
// never matters.
//
// # SLO gates
//
// Non-streaming: p99 overhead ≤ 5 ms, p50 ≤ 1 ms (CLAUDE.md performance contract).
//
// Streaming: p99 overhead ≤ 10 ms, p50 ≤ 2 ms. The roomier budget reflects that
// the streaming path involves multiple write syscalls and Flush() calls — one per
// SSE chunk — plus a tee goroutine reading the body copy for usage extraction. Each
// flush round-trips through the OS kernel; 5–20 chunks means 5–20× the write
// pressure of a single JSON response. The 10 ms / 2 ms SLO is therefore conservative
// relative to the non-streaming budget on a per-syscall basis while still catching
// regressions in Relay's own orchestration layer.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"sort"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	apiopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/auth"
	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/eventlog"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/limit"
	"github.com/wyolet/relay/pkg/pipeline"
	"github.com/wyolet/relay/pkg/provider"
	providerOpenAI "github.com/wyolet/relay/pkg/provider/openai"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/transport"
	"github.com/wyolet/relay/pkg/usage"
)

// chatInput mirrors the production input struct in cmd/relay/openapi.go.
type chatInput struct {
	RawBody json.RawMessage `doc:"OpenAI-compatible chat completion request."`
}

// cannedResponse is a realistic ~500-byte non-streaming chat completion response.
const cannedResponse = `{"id":"chatcmpl-bench001","object":"chat.completion","created":1700000000,"model":"gpt-bench","choices":[{"index":0,"message":{"role":"assistant","content":"The answer is 42. This is a canned response used by the Relay p99 bench to provide a realistic payload size without a live upstream."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":32,"total_tokens":44}}`

// benchKey is the fixed bearer token used in bench requests.
const benchKey = "bench-test-key-0001"

// stubUpstream returns an httptest.Server that immediately responds with
// cannedResponse for any request, mimicking a fast upstream provider.
func stubUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedResponse))
	}))
}

// benchResult holds percentile latencies and iteration count, written to results.json.
// Path tags which hot path the measurement covers ("chat" or "streaming").
// Mode tags the parsing mode ("rich" or "minimal") for chat-path measurements; empty for streaming.
type benchResult struct {
	Path   string `json:"path"`
	Mode   string `json:"mode,omitempty"`
	P50us  int64  `json:"p50_us"`
	P95us  int64  `json:"p95_us"`
	P99us  int64  `json:"p99_us"`
	N      int    `json:"n"`
	GitSHA string `json:"git_sha"`
}

// gitSHA returns the vcs.revision build setting, GITHUB_SHA env var, or "unknown".
func gitSHA() string {
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return sha
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// percentile returns the p-th percentile (0–100) of a sorted slice of int64.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

// buildRelayHandler constructs an http.Handler that mirrors main.go's wiring
// with the lightest possible config:
//   - configstore: programmatic in-memory MemStore pointing at stubURL
//   - state: in-memory (state.New)
//   - eventlog: file backend in t.TempDir()
//   - usage: no-op OTel (no OTLP endpoint)
//   - auth: single fixed key (benchKey)
func buildRelayHandler(tb testing.TB, stubURL string) http.Handler {
	tb.Helper()

	const (
		providerName = "bench-provider"
		modelName    = "gpt-bench"
		poolName     = "bench-pool"
		secretName   = "bench-secret"
	)

	prov := &configstore.Provider{
		APIVersion: configstore.APIVersion,
		Kind:       configstore.KindProvider,
		Metadata:   configstore.Metadata{Name: providerName},
		Spec: configstore.ProviderSpec{
			Kind:        configstore.PKOpenAI,
			BaseURL:     stubURL,
			Default:     true,
			DefaultPool: poolName,
		},
	}
	sec := &configstore.Secret{
		APIVersion: configstore.APIVersion,
		Kind:       configstore.KindSecret,
		Metadata:   configstore.Metadata{Name: secretName},
		Spec: configstore.SecretSpec{
			Provider: providerName,
			Value:    "bench-api-key",
		},
		Resolved: "bench-api-key",
		KeyHash:  "benchhash",
	}
	pool := &configstore.Pool{
		APIVersion: configstore.APIVersion,
		Kind:       configstore.KindPool,
		Metadata:   configstore.Metadata{Name: poolName},
		Spec: configstore.PoolSpec{
			Provider: providerName,
			Secrets:  []string{secretName},
		},
	}
	model := &configstore.Model{
		APIVersion: configstore.APIVersion,
		Kind:       configstore.KindModel,
		Metadata:   configstore.Metadata{Name: modelName},
		Spec: configstore.ModelSpec{
			Provider:     providerName,
			UpstreamName: "gpt-bench",
		},
	}

	cfg := configstore.NewMemStore(prov, sec, pool, model)
	st := kv.NewMem()

	el, err := eventlog.New(eventlog.Config{
		Backend: eventlog.BackendFile,
		Dir:     tb.TempDir(),
	})
	if err != nil {
		tb.Fatalf("eventlog.New: %v", err)
	}

	// No-op OTel: OTLPEndpoint is empty.
	if _, err = usage.Init(context.Background(), usage.Config{
		EventLog:        el,
		CatalogBackend:  "memory",
		StateBackend:    "memory",
		EventlogBackend: "file",
	}); err != nil {
		tb.Fatalf("usage.Init: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(configstore.PKOpenAI, providerOpenAI.New(stubURL))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := limit.New(st, logger, nil)
	sel := keypool.New(st, logger, nil, limiter, cfg, nil)

	resolve := func(modelAlias string) (*apiopenai.RequestPlan, bool) {
		m, ok := cfg.ModelByName(modelAlias)
		if !ok {
			return nil, false
		}
		p, ok := cfg.ProviderForModel(modelAlias)
		if !ok {
			return nil, false
		}
		plan := &apiopenai.RequestPlan{Model: m, Provider: p}
		if poolN := p.Spec.DefaultPool; poolN != "" {
			if pl, ok2 := cfg.PoolByName(poolN); ok2 {
				plan.Pool = pl
				plan.Secrets = cfg.SecretsForPool(pl)
				plan.Rules = cfg.RateLimitsForRequest(p, pl, m, nil)
			}
		}
		return plan, true
	}

	runPipeline := func(ctx context.Context, ch *transport.Channel, plan *apiopenai.RequestPlan) error {
		ob, err := reg.Get(plan.Provider.Spec.Kind)
		if err != nil {
			return err
		}
		if plan.Pool != nil && len(plan.Secrets) > 0 {
			return pipeline.Run(ctx, ch, pipeline.RunOptions{
				Provider: plan.Provider,
				Pool:     plan.Pool,
				Model:    plan.Model,
				Secrets:  plan.Secrets,
				Selector: sel,
				Outbound: ob,
				Limiter:  limiter,
				Rules:    plan.Rules,
			})
		}
		emptySecret := &configstore.Secret{
			Metadata: configstore.Metadata{Name: "anon"},
			Resolved: "",
			KeyHash:  "anon",
		}
		syntheticPool := &configstore.Pool{
			Metadata: configstore.Metadata{Name: "anon-pool"},
		}
		return pipeline.Run(ctx, ch, pipeline.RunOptions{
			Pool:     syntheticPool,
			Secrets:  []*configstore.Secret{emptySecret},
			Selector: sel,
			Outbound: ob,
		})
	}

	apiKeys := auth.ParseKeys(benchKey)
	authMW := auth.Middleware(apiKeys)

	r := chi.NewRouter()
	r.Use(reqid.Middleware(logger))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	mountBenchHuma(r, authMW, apiopenai.ChatCompletions(resolve, runPipeline), apiopenai.ListModels(cfg))

	return r
}

// humaAuthBench converts a net/http middleware into a huma per-operation
// middleware, mirroring humaAuth in cmd/relay/openapi.go.
func humaAuthBench(authMW func(http.Handler) http.Handler) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humachi.Unwrap(ctx)
		authMW(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			next(humachi.NewContext(ctx.Operation(), r2, w2))
		})).ServeHTTP(w, r)
	}
}

// mountBenchHuma wires the chi router through huma + humachi exactly as
// cmd/relay/openapi.go does: chat is registered as a huma operation with a
// RawBody input struct; the body-restore pattern from delegateBody (PER-256)
// ensures the downstream handler can re-read r.Body after huma consumes it.
func mountBenchHuma(
	chiRouter chi.Router,
	authMW func(http.Handler) http.Handler,
	chatH http.HandlerFunc,
	modelsH http.HandlerFunc,
) {
	cfg := huma.DefaultConfig("Wyolet Relay bench", "0.0.0")
	api := humachi.New(chiRouter, cfg)
	auth := huma.Middlewares{humaAuthBench(authMW)}

	delegate := func(h http.HandlerFunc) func(context.Context, *struct{}) (*huma.StreamResponse, error) {
		return func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{
				Body: func(ctx huma.Context) {
					r, w := humachi.Unwrap(ctx)
					h.ServeHTTP(w, r)
				},
			}, nil
		}
	}

	// POST /v1/chat/completions — huma reads RawBody, we restore r.Body so
	// ChatCompletions can re-read it (mirrors delegateBody in openapi.go).
	huma.Register(api, huma.Operation{
		OperationID: "create-chat-completion",
		Method:      http.MethodPost,
		Path:        "/v1/chat/completions",
		Summary:     "Create chat completion",
		Tags:        []string{"chat"},
		Errors:      []int{400, 401, 404, 429, 500},
		Middlewares: auth,
	}, func(_ context.Context, inp *chatInput) (*huma.StreamResponse, error) {
		raw := inp.RawBody
		return &huma.StreamResponse{
			Body: func(ctx huma.Context) {
				r, w := humachi.Unwrap(ctx)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
				chatH.ServeHTTP(w, r)
			},
		}, nil
	})

	// GET /v1/models — auth-gated.
	huma.Register(api, huma.Operation{
		OperationID: "list-models",
		Method:      http.MethodGet,
		Path:        "/v1/models",
		Summary:     "List models",
		Tags:        []string{"models"},
		Errors:      []int{401},
		Middlewares: auth,
	}, delegate(modelsH))
}

// BenchmarkBareStub measures the cost of calling the stub handler directly via
// ServeHTTP (no TCP, no Relay code), providing a floor for comparison.
func BenchmarkBareStub(b *testing.B) {
	stub := stubUpstream()
	b.Cleanup(stub.Close)

	handler := stub.Config.Handler
	body := benchBody()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}
}

// runChatOverhead is the shared implementation for BenchmarkRelayChatOverhead_Rich
// and BenchmarkRelayChatOverhead_Minimal. mode is "rich" or "minimal".
func runChatOverhead(b *testing.B, mode string) {
	b.Helper()

	stub := stubUpstream()
	b.Cleanup(stub.Close)

	handler := buildRelayHandler(b, stub.URL)
	body := benchBody()

	stubHandler := stub.Config.Handler
	const warmupN = 500
	stubNs := make([]int64, 0, warmupN)
	for i := 0; i < warmupN; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		t0 := time.Now()
		stubHandler.ServeHTTP(rr, req)
		stubNs = append(stubNs, time.Since(t0).Nanoseconds())
	}
	sort.Slice(stubNs, func(i, j int) bool { return stubNs[i] < stubNs[j] })
	stubP50ns := percentile(stubNs, 50)

	b.ResetTimer()

	relayNs := make([]int64, 0, b.N)
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+benchKey)

		rr := httptest.NewRecorder()
		t0 := time.Now()
		handler.ServeHTTP(rr, req)
		elapsed := time.Since(t0).Nanoseconds()

		if rr.Code != http.StatusOK {
			b.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
		}
		relayNs = append(relayNs, elapsed)
	}

	b.StopTimer()

	if b.N < 100 {
		return
	}

	sort.Slice(relayNs, func(i, j int) bool { return relayNs[i] < relayNs[j] })

	overheadNs := make([]int64, len(relayNs))
	for i, lat := range relayNs {
		if ov := lat - stubP50ns; ov > 0 {
			overheadNs[i] = ov
		}
	}
	sort.Slice(overheadNs, func(i, j int) bool { return overheadNs[i] < overheadNs[j] })

	p50 := percentile(overheadNs, 50) / 1000
	p95 := percentile(overheadNs, 95) / 1000
	p99 := percentile(overheadNs, 99) / 1000

	b.Logf("mode=%s stub p50=%dns | relay-total p50=%dµs p95=%dµs p99=%dµs",
		mode, stubP50ns,
		percentile(relayNs, 50)/1000,
		percentile(relayNs, 95)/1000,
		percentile(relayNs, 99)/1000,
	)
	b.Logf("mode=%s overhead (relay - stub_p50): p50=%dµs p95=%dµs p99=%dµs n=%d",
		mode, p50, p95, p99, len(overheadNs))

	result := benchResult{
		Path:   "chat",
		Mode:   mode,
		P50us:  p50,
		P95us:  p95,
		P99us:  p99,
		N:      len(overheadNs),
		GitSHA: gitSHA(),
	}
	writeResults(b, mode, result)

	if p99 > 5000 {
		b.Fatalf("SLO BREACH: p99 overhead %dµs > 5000µs (5ms) [mode=%s]", p99, mode)
	}
	if p50 > 1000 {
		b.Fatalf("SLO BREACH: p50 overhead %dµs > 1000µs (1ms) [mode=%s]", p50, mode)
	}

	fmt.Printf("Relay chat overhead [%s] — p50: %dµs  p95: %dµs  p99: %dµs  n: %d\n",
		mode, p50, p95, p99, len(overheadNs))
}

// BenchmarkRelayChatOverhead_Rich is the chat SLO gate for the M7 default parsing mode.
//
// It sets RELAY_RICH_PARSING=on (full body parse: model, stream, user, metadata
// validation, messages extraction) and measures end-to-end overhead through the
// full Relay handler. Fails the build when p99 > 5 ms or p50 > 1 ms.
func BenchmarkRelayChatOverhead_Rich(b *testing.B) {
	prev := apiopenai.RichParsing()
	apiopenai.SetRichParsing(true)
	b.Cleanup(func() { apiopenai.SetRichParsing(prev) })
	runChatOverhead(b, "rich")
}

// BenchmarkRelayChatOverhead_Minimal is the chat SLO gate for the legacy partial-parse path.
//
// It sets RELAY_RICH_PARSING=off (model/stream/user/raw only; metadata and messages
// left nil) and measures end-to-end overhead through the full Relay handler.
// Fails the build when p99 > 5 ms or p50 > 1 ms.
func BenchmarkRelayChatOverhead_Minimal(b *testing.B) {
	prev := apiopenai.RichParsing()
	apiopenai.SetRichParsing(false)
	b.Cleanup(func() { apiopenai.SetRichParsing(prev) })
	runChatOverhead(b, "minimal")
}

// benchBody returns a minimal but valid chat-completions request body.
func benchBody() []byte {
	body := map[string]any{
		"model": "gpt-bench",
		"messages": []map[string]string{
			{"role": "user", "content": "What is 6×7?"},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// writeResults writes bench/results-{mode}.json in the bench directory and also
// bench/results.json for the last mode written (CI reads both files by mode name).
// Failures are non-fatal (they don't break the bench gate itself).
func writeResults(tb testing.TB, mode string, r benchResult) {
	tb.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		tb.Logf("writeResults: marshal: %v", err)
		return
	}
	modeFile := "results-" + mode + ".json"
	if err := os.WriteFile(modeFile, data, 0o644); err != nil {
		tb.Logf("writeResults: write %s: %v", modeFile, err)
	}
}

// streamingBenchResult holds percentile latencies for the streaming path.
type streamingBenchResult struct {
	Path   string `json:"path"`
	P50us  int64  `json:"p50_us"`
	P95us  int64  `json:"p95_us"`
	P99us  int64  `json:"p99_us"`
	N      int    `json:"n"`
	GitSHA string `json:"git_sha"`
}

// writeStreamingResults writes streaming-results.json in the bench directory.
func writeStreamingResults(tb testing.TB, r streamingBenchResult) {
	tb.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		tb.Logf("writeStreamingResults: marshal: %v", err)
		return
	}
	if err := os.WriteFile("streaming-results.json", data, 0o644); err != nil {
		tb.Logf("writeStreamingResults: write: %v", err)
	}
}

// cannedSSEChunk is a single realistic chat-completions streaming delta chunk.
const cannedSSEChunk = `data: {"id":"chatcmpl-stream001","object":"chat.completion.chunk","created":1700000000,"model":"gpt-bench","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n"

// cannedSSEDone is the terminal SSE frame.
const cannedSSEDone = "data: [DONE]\n\n"

// stubStreamingUpstream returns an httptest.Server that responds with a
// multi-chunk SSE response: 10 data chunks followed by [DONE]. Each chunk is
// flushed individually to exercise the streaming write path.
func stubStreamingUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 10; i++ {
			_, _ = w.Write([]byte(cannedSSEChunk))
			flusher.Flush()
		}
		_, _ = w.Write([]byte(cannedSSEDone))
		flusher.Flush()
	}))
}

// BenchmarkBareStubStreaming measures the cost of calling the streaming stub
// handler directly via ServeHTTP (no TCP, no Relay code), providing a floor
// for the streaming overhead comparison.
func BenchmarkBareStubStreaming(b *testing.B) {
	stub := stubStreamingUpstream()
	b.Cleanup(stub.Close)

	handler := stub.Config.Handler
	body := benchBody()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}
}

// BenchmarkRelayStreamingOverhead is the streaming SLO gate.
//
// It measures end-to-end latency from request entry to final chunk delivered
// through the full Relay handler (auth + body-limit + reqid + huma routing +
// model resolution + key selection + pipeline), using a multi-chunk SSE stub
// upstream. Subtracts a bare-stub streaming baseline to isolate Relay's own
// contribution. Hard-fails when p99 > 10 ms or p50 > 2 ms.
//
// The roomier thresholds vs. the non-streaming gate reflect that the streaming
// path issues one write + Flush per chunk (10 in the stub), multiplying syscall
// pressure. See the package doc for full rationale.
func BenchmarkRelayStreamingOverhead(b *testing.B) {
	stub := stubStreamingUpstream()
	b.Cleanup(stub.Close)

	handler := buildRelayHandler(b, stub.URL)
	body := benchBody()

	stubHandler := stub.Config.Handler
	const warmupN = 500
	stubNs := make([]int64, 0, warmupN)
	for i := 0; i < warmupN; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		t0 := time.Now()
		stubHandler.ServeHTTP(rr, req)
		stubNs = append(stubNs, time.Since(t0).Nanoseconds())
	}
	sort.Slice(stubNs, func(i, j int) bool { return stubNs[i] < stubNs[j] })
	stubP50ns := percentile(stubNs, 50)

	b.ResetTimer()

	relayNs := make([]int64, 0, b.N)
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+benchKey)

		rr := httptest.NewRecorder()
		t0 := time.Now()
		handler.ServeHTTP(rr, req)
		elapsed := time.Since(t0).Nanoseconds()

		if rr.Code != http.StatusOK {
			b.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
		}
		relayNs = append(relayNs, elapsed)
	}

	b.StopTimer()

	if b.N < 100 {
		return
	}

	sort.Slice(relayNs, func(i, j int) bool { return relayNs[i] < relayNs[j] })

	overheadNs := make([]int64, len(relayNs))
	for i, lat := range relayNs {
		if ov := lat - stubP50ns; ov > 0 {
			overheadNs[i] = ov
		}
	}
	sort.Slice(overheadNs, func(i, j int) bool { return overheadNs[i] < overheadNs[j] })

	p50 := percentile(overheadNs, 50) / 1000
	p95 := percentile(overheadNs, 95) / 1000
	p99 := percentile(overheadNs, 99) / 1000

	b.Logf("streaming stub p50=%dns | relay-total p50=%dµs p95=%dµs p99=%dµs",
		stubP50ns,
		percentile(relayNs, 50)/1000,
		percentile(relayNs, 95)/1000,
		percentile(relayNs, 99)/1000,
	)
	b.Logf("streaming overhead (relay - stub_p50): p50=%dµs p95=%dµs p99=%dµs n=%d",
		p50, p95, p99, len(overheadNs))

	result := streamingBenchResult{
		Path:   "streaming",
		P50us:  p50,
		P95us:  p95,
		P99us:  p99,
		N:      len(overheadNs),
		GitSHA: gitSHA(),
	}
	writeStreamingResults(b, result)

	if p99 > 10000 {
		b.Fatalf("SLO BREACH: streaming p99 overhead %dµs > 10000µs (10ms)", p99)
	}
	if p50 > 2000 {
		b.Fatalf("SLO BREACH: streaming p50 overhead %dµs > 2000µs (2ms)", p50)
	}

	fmt.Printf("Relay streaming overhead — p50: %dµs  p95: %dµs  p99: %dµs  n: %d\n",
		p50, p95, p99, len(overheadNs))
}
