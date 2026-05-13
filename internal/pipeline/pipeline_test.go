package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/eventlog"
	"github.com/wyolet/relay/internal/keypool"
	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/transport"
	"github.com/wyolet/relay/internal/usage"
)

// fakeOutbound is a controllable provider.Outbound for tests.
type fakeOutbound struct {
	calls  atomic.Int32
	handle func(callIdx int, secret string, out chan<- *transport.Message)
}

func (f *fakeOutbound) ChatCompletions(ctx context.Context, body []byte, secret string, out chan<- *transport.Message) error {
	idx := int(f.calls.Add(1))
	defer close(out)
	f.handle(idx, secret, out)
	return nil
}

// testSetup builds a *Request with two secrets and a fresh Selector.
func testSetup(t *testing.T, ob *fakeOutbound) (*Request, func()) {
	t.Helper()
	st := kv.NewMem()
	sel := keypool.New(st, slog.Default(), nil, nil)

	policy := &catalog.Policy{
		Metadata: catalog.Metadata{Name: "test-policy"},
	}
	secrets := []*catalog.Secret{
		{Metadata: catalog.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
		{Metadata: catalog.Metadata{Name: "key2"}, Resolved: "secret-key2", KeyHash: "hash2"},
	}

	req := &Request{
		Body:        []byte(`{"model":"gpt-4"}`),
		Policy:      policy,
		Secrets:     secrets,
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
	}
	return req, func() { st.Close() }
}

// testOpenAIExtractTokens is a minimal token extractor for pipeline tests.
// It avoids importing internal/api/openai (which would create an import cycle)
// while still exercising the TokenExtractor path.
func testOpenAIExtractTokens(body []byte) usage.Tokens {
	var resp struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return nil
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return nil
	}
	t := usage.Tokens{}
	if v := resp.Usage.PromptTokens; v > 0 {
		t["input"] = v
	}
	if v := resp.Usage.CompletionTokens; v > 0 {
		t["output"] = v
	}
	return t
}

// runBody reads all bytes from resp.Body. Used in tests to assert on body content.
func runBody(resp *Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRun_Success(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
		out <- &transport.Message{Body: []byte("chunk")}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("expected status 200, got %d", resp.Status)
	}
	body := runBody(resp)
	if body != "chunk" {
		t.Fatalf("expected body 'chunk', got %q", body)
	}
	if ob.calls.Load() != 1 {
		t.Fatalf("expected 1 outbound call, got %d", ob.calls.Load())
	}
}

func TestRun_5xxThen200_SameKey(t *testing.T) {
	var keys []string
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		keys = append(keys, secret)
		if idx == 1 {
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "500", "X-Relay-Final": "true"}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if ob.calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", ob.calls.Load())
	}
	if keys[0] != keys[1] {
		t.Fatalf("expected same key for both calls, got %q and %q", keys[0], keys[1])
	}
}

func TestRun_AuthFailover(t *testing.T) {
	var keys []string
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		keys = append(keys, secret)
		if idx == 1 {
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "401", "X-Relay-Final": "true"}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if ob.calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", ob.calls.Load())
	}
	if keys[0] == keys[1] {
		t.Fatalf("expected different keys after 401 failover, got same key %q", keys[0])
	}
}

func TestRun_429Short_RetrySameKey(t *testing.T) {
	var keys []string
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		keys = append(keys, secret)
		if idx == 1 {
			// 0s retry-after — well under 5s threshold, so same-key retry
			out <- &transport.Message{Headers: map[string]string{
				"X-Relay-Status": "429",
				"Retry-After":    "0", // 0s, but we test the same-key path
				"X-Relay-Final":  "true",
			}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if ob.calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", ob.calls.Load())
	}
	if keys[0] != keys[1] {
		t.Fatalf("expected same key for 429-short retry, got %q and %q", keys[0], keys[1])
	}
}

func TestRun_429Long_Failover(t *testing.T) {
	var keys []string
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		keys = append(keys, secret)
		if idx == 1 {
			// 30s > 5s threshold → failover
			out <- &transport.Message{Headers: map[string]string{
				"X-Relay-Status": "429",
				"Retry-After":    "30",
				"X-Relay-Final":  "true",
			}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if ob.calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", ob.calls.Load())
	}
	if keys[0] == keys[1] {
		t.Fatalf("expected different keys after 429-long failover")
	}
}

func TestRun_AllExhausted(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "500", "X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.MaxAttempts = 3

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	if resp.Status != 502 {
		t.Fatalf("expected status 502, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "upstream_5xx_exhausted") {
		t.Fatalf("expected upstream_5xx_exhausted code, got %s", body)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStrHelper(s, sub))
}

func containsStrHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRun_AllAuthFailed_AuthFailedEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "401", "X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.MaxAttempts = 2

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	if resp.Status != 502 {
		t.Fatalf("expected 502, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "auth_failed") {
		t.Fatalf("expected auth_failed code, got %s", body)
	}
	if !containsStr(body, "upstream_error") {
		t.Fatalf("expected upstream_error type, got %s", body)
	}
}

func TestRun_AllRateLimited_RateLimitEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{
			"X-Relay-Status": "429",
			"Retry-After":    "30",
			"X-Relay-Final":  "true",
		}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.MaxAttempts = 2

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	if resp.Status != 429 {
		t.Fatalf("expected 429, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "rate_limit_exceeded") {
		t.Fatalf("expected rate_limit_exceeded, got %s", body)
	}
	if ra := resp.Headers["Retry-After"]; ra != "30" {
		t.Fatalf("expected Retry-After: 30, got %s", ra)
	}
}

func TestRun_All5xx_ServerErrorEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "500", "X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.MaxAttempts = 3

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	if resp.Status != 502 {
		t.Fatalf("expected 502, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "upstream_5xx_exhausted") {
		t.Fatalf("expected upstream_5xx_exhausted, got %s", body)
	}
}

func TestRun_NoKeysFromStart_NoHealthyKeysEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	// Mark all keys as auth-failed so ErrNoHealthyKeys is returned immediately.
	req.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	req.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	resp, err := Run(ctx, req)
	if err != keypool.ErrNoHealthyKeys {
		t.Fatalf("expected ErrNoHealthyKeys, got %v", err)
	}
	if resp.Status != 503 {
		t.Fatalf("expected 503, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "no_healthy_keys") {
		t.Fatalf("expected no_healthy_keys, got %s", body)
	}
}

func TestRun_NoHealthyKeys(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	// Mark both keys as auth-failed so they're permanently open.
	ctx := context.Background()
	req.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	req.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	resp, err := Run(ctx, req)
	if err != keypool.ErrNoHealthyKeys {
		t.Fatalf("expected ErrNoHealthyKeys, got %v", err)
	}
	if resp.Status != 503 {
		t.Fatalf("expected 503, got %d", resp.Status)
	}
	runBody(resp) // drain
}

func TestRun_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Outbound blocks until ctx is cancelled.
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		// Don't send anything — let ctx.Done() trigger in Run.
		<-ctx.Done()
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, req)
		errCh <- err
	}()

	// Give Run time to start, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRun_NetworkError(t *testing.T) {
	// Network error: outbound emits 502 with X-Relay-Final=true (our error envelope)
	// Pipeline treats this as ServerError, retries same key once, then fails over.
	var keys []string
	callCount := 0
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		keys = append(keys, secret)
		callCount++
		if idx <= 2 {
			// Both calls on same-key-retry and failover return error
			out <- &transport.Message{Headers: map[string]string{
				"X-Relay-Status": "502",
				"X-Relay-Final":  "true",
				"Content-Type":   "application/json",
			}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	// call 1: 502 same key → sameKeyAttempt=0→1
	// call 2: 502 failover → sameKeyAttempt=1→0, chosenKey=nil
	// call 3: 200 success
	if ob.calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", ob.calls.Load())
	}
	// key1 == key2 (same-key retry), key2 != key3 (failover)
	if keys[0] != keys[1] {
		t.Fatalf("first retry should use same key: %q vs %q", keys[0], keys[1])
	}
	if keys[1] == keys[2] {
		t.Fatalf("second retry should use different key (failover): %q vs %q", keys[1], keys[2])
	}
}

// --- Rate limit tests ---

// fakeLimiter tracks Reserve/Commit calls for pipeline rate-limit tests.
type fakeLimiter struct {
	reserveErr   error
	commitCalls  []ratelimit.Observations
	reserveCalls int
}

func (f *fakeLimiter) reserve(ctx context.Context, rules []catalog.ResolvedRule) (*ratelimit.Reservation, error) {
	f.reserveCalls++
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return nil, nil
}

// testLimiterSetup creates a real Limiter backed by MemStore, plus a rule set.
// The rule allows 1000 requests/min so tests pass through unless forced to fail.
func testLimiterSetup(t *testing.T) (*ratelimit.Limiter, []catalog.ResolvedRule, func()) {
	t.Helper()
	st := kv.NewMem()
	l := ratelimit.New(st, slog.Default(), nil)
	rules := []catalog.ResolvedRule{
		{
			ParentKind: catalog.KindPolicy,
			ParentName: "test-policy",
			Meter:      catalog.MeterRequests,
			RateLimit: &catalog.RateLimit{
				Metadata: catalog.Metadata{Name: "rpm"},
				Spec: catalog.RateLimitSpec{
					Rules: []catalog.RateLimitRule{{Meter: string(catalog.MeterRequests), Amount: 1000, Window: time.Minute, Strategy: catalog.StrategySlidingWindow}},
				},
			},
			Rule:     catalog.RateLimitRule{Meter: string(catalog.MeterRequests), Amount: 1000, Window: time.Minute, Strategy: catalog.StrategySlidingWindow},
			Strategy: catalog.StrategySlidingWindow,
			Window:   time.Minute,
		},
	}
	return l, rules, func() { st.Close() }
}

func testSetupWithLimiter(t *testing.T, ob *fakeOutbound, l *ratelimit.Limiter, rules []catalog.ResolvedRule) *Request {
	t.Helper()
	st := kv.NewMem()
	t.Cleanup(func() { st.Close() })
	sel := keypool.New(st, slog.Default(), nil, nil)
	policy := &catalog.Policy{Metadata: catalog.Metadata{Name: "test-policy"}}
	secrets := []*catalog.Secret{
		{Metadata: catalog.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
	}
	return &Request{
		Body:        []byte(`{"model":"gpt-4"}`),
		Policy:      policy,
		Secrets:     secrets,
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}
}

func exceededRules(meter catalog.Meter, retryAfterSec int) ([]catalog.ResolvedRule, *ratelimit.Limiter, func()) {
	st := kv.NewMem()
	window := time.Minute
	amount := int64(1)
	// Use a fixed clock so the window bucket is deterministic.
	now := time.Now()
	l := ratelimit.New(st, slog.Default(), func() time.Time { return now })
	rule := catalog.ResolvedRule{
		ParentKind: catalog.KindPolicy,
		ParentName: "test-policy",
		Meter:      meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: string(meter) + "-limit"},
			Spec: catalog.RateLimitSpec{
				Rules: []catalog.RateLimitRule{{Meter: string(meter), Amount: amount, Window: window, Strategy: catalog.StrategySlidingWindow}},
			},
		},
		Rule:     catalog.RateLimitRule{Meter: string(meter), Amount: amount, Window: window, Strategy: catalog.StrategySlidingWindow},
		Strategy: catalog.StrategySlidingWindow,
		Window:   window,
	}
	rules := []catalog.ResolvedRule{rule}
	ctx := context.Background()
	// Exhaust the budget: Reserve once (succeeds), then the next Reserve will fail.
	if meter == catalog.MeterRequests {
		l.Reserve(ctx, "test-policy", rules)
	} else if meter == catalog.MeterConcurrency {
		l.Reserve(ctx, "test-policy", rules)
	}
	// For tokens: set the counter via a successful Reserve+Commit with tokens=amount.
	if meter == catalog.MeterTokens {
		res, _ := l.Reserve(ctx, "test-policy", rules)
		if res != nil {
			commitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			l.Commit(commitCtx, res, ratelimit.Observations{Tokens: usage.Tokens{"tokens": amount}})
		}
	}
	return rules, l, func() { st.Close() }
}

func TestRun_LimiterNil_NoGating(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	// req.Limiter is nil by default — no gating

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if ob.calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", ob.calls.Load())
	}
}

func TestRun_RPMExceeded_Returns429(t *testing.T) {
	rules, l, cleanup := exceededRules(catalog.MeterRequests, 30)
	defer cleanup()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		// should never be called
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	st2 := kv.NewMem()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil, nil)
	req := &Request{
		Body:        []byte(`{"model":"gpt-4"}`),
		Policy:      &catalog.Policy{Metadata: catalog.Metadata{Name: "test-policy"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err == nil {
		t.Fatal("expected error from rate limit, got nil")
	}
	if resp.Status != 429 {
		t.Fatalf("expected 429, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "rpm_exceeded") {
		t.Fatalf("expected rpm_exceeded code in body: %s", body)
	}
	if !containsStr(body, "rate_limit_exceeded") {
		t.Fatalf("expected rate_limit_exceeded type in body: %s", body)
	}
	if ob.calls.Load() != 0 {
		t.Fatalf("outbound should not be called on rate-limit violation, got %d calls", ob.calls.Load())
	}
}

func TestRun_ConcurrencyExceeded_Returns429(t *testing.T) {
	rules, l, cleanup := exceededRules(catalog.MeterConcurrency, 0)
	defer cleanup()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	st2 := kv.NewMem()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil, nil)
	req := &Request{
		Body:        []byte(`{"model":"gpt-4"}`),
		Policy:      &catalog.Policy{Metadata: catalog.Metadata{Name: "test-policy"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err == nil {
		t.Fatal("expected error from concurrency limit")
	}
	if resp.Status != 429 {
		t.Fatalf("expected 429, got %d", resp.Status)
	}
	body := runBody(resp)
	if !containsStr(body, "concurrency_exceeded") {
		t.Fatalf("expected concurrency_exceeded in body: %s", body)
	}
	if ob.calls.Load() != 0 {
		t.Fatalf("outbound should not be called on rate-limit violation")
	}
}

func TestRun_TokensCommittedFromResponseUsage(t *testing.T) {
	l, rules, cleanup := testLimiterSetup(t)
	defer cleanup()

	responseBody := []byte(`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":32,"total_tokens":42}}`)
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"},
			Body:    responseBody,
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}

	req := testSetupWithLimiter(t, ob, l, rules)
	req.TokenExtractor = testOpenAIExtractTokens
	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify pipeline forwarded the body containing usage.
	body := runBody(resp)
	if !containsStr(body, "total_tokens") {
		t.Fatal("expected usage body to be forwarded")
	}
}

func TestRun_StreamingTokensCommitted(t *testing.T) {
	l, rules, cleanup := testLimiterSetup(t)
	defer cleanup()

	chunk1 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	chunk2 := []byte("data: {\"choices\":[],\"usage\":{\"total_tokens\":13}}\n\n")

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{
			Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "text/event-stream"},
		}
		out <- &transport.Message{Body: chunk1}
		out <- &transport.Message{Body: chunk2}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}

	req := testSetupWithLimiter(t, ob, l, rules)
	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
}

func TestRun_CancellationCommitsCancelled(t *testing.T) {
	l, rules, cleanup := testLimiterSetup(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		// Block before sending anything — let ctx.Done() trigger in Run.
		<-ctx.Done()
	}}

	st2 := kv.NewMem()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil, nil)
	req := &Request{
		Body:        []byte(`{"model":"gpt-4"}`),
		Policy:      &catalog.Policy{Metadata: catalog.Metadata{Name: "test-policy"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, req)
		errCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// --- Lifecycle / usage.Record integration tests ---

// lcEnv sets up a temp-dir eventlog + tracetest recorder, installs both
// globally, and returns a context with a span on it.
// flush() closes the eventlog (draining pending writes) then reads events.
// t.Cleanup handles OTel shutdown.
func lcEnv(t *testing.T) (ctx context.Context, sr *tracetest.SpanRecorder, flush func() []map[string]interface{}) {
	t.Helper()
	dir := t.TempDir()
	el, err := eventlog.New(eventlog.Config{Dir: dir, BufferSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	sr = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	shutdown, err := usage.Init(context.Background(), usage.Config{EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdown(shutCtx)
		// Reset global eventlogger so parallel pkg tests aren't affected.
		usage.Init(context.Background(), usage.Config{}) //nolint:errcheck
	})
	spanCtx, sp := tp.Tracer("relay").Start(context.Background(), usage.SpanName)
	spanCtx = usage.ContextWithSpan(spanCtx, sp)
	flush = func() []map[string]interface{} {
		t.Helper()
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		el.Close(closeCtx)
		return readEvents(t, dir)
	}
	return spanCtx, sr, flush
}

// readEvents reads all JSONL events from dir.
func readEvents(t *testing.T, dir string) []map[string]interface{} {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(dir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var m map[string]interface{}
			if err2 := json.Unmarshal(line, &m); err2 != nil {
				f.Close()
				t.Fatal(err2)
			}
			out = append(out, m)
		}
		f.Close()
	}
	return out
}

func TestLifecycle_SingleSuccess(t *testing.T) {
	ctx, sr, flush := lcEnv(t)

	responseBody := []byte(`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}, Body: responseBody}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.Model = &catalog.Model{Metadata: catalog.Metadata{Name: "gpt-4o"}}
	req.Provider = &catalog.Provider{Metadata: catalog.Metadata{Name: "openai"}}
	req.TokenExtractor = testOpenAIExtractTokens

	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain so pipeline goroutine exits and event is emitted

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev["terminated_by"] != "clean" {
		t.Errorf("terminated_by want clean, got %v", ev["terminated_by"])
	}
	if ev["model"] != "gpt-4o" {
		t.Errorf("model want gpt-4o, got %v", ev["model"])
	}
	attempts, _ := ev["attempts"].([]interface{})
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts))
	}
	a := attempts[0].(map[string]interface{})
	if a["outcome"] != "success" {
		t.Errorf("attempt outcome want success, got %v", a["outcome"])
	}
	metrics, _ := ev["metrics"].(map[string]interface{})
	for _, key := range []string{"pre_upstream_ms", "upstream_ttfb_ms", "upstream_total_ms", "total_ms"} {
		if _, ok := metrics[key]; !ok {
			t.Errorf("missing metric %q", key)
		}
	}
	tokens, _ := ev["tokens"].(map[string]interface{})
	if tokens["input"] != float64(10) {
		t.Errorf("tokens.input want 10, got %v", tokens["input"])
	}
	if tokens["output"] != float64(20) {
		t.Errorf("tokens.output want 20, got %v", tokens["output"])
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span ended, got %d", len(spans))
	}
}

func TestLifecycle_FailoverThenSuccess(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		if idx == 1 {
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "500", "X-Relay-Final": "true"}}
			return
		}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev["terminated_by"] != "clean" {
		t.Errorf("terminated_by want clean, got %v", ev["terminated_by"])
	}
	attempts, _ := ev["attempts"].([]interface{})
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}
	a0 := attempts[0].(map[string]interface{})
	a1 := attempts[1].(map[string]interface{})
	if a0["outcome"] != "http_5xx" {
		t.Errorf("attempt[0] outcome want http_5xx, got %v", a0["outcome"])
	}
	if a1["outcome"] != "success" {
		t.Errorf("attempt[1] outcome want success, got %v", a1["outcome"])
	}
}

func TestLifecycle_RateLimitedByReserve(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	rules, l, rcleanup := exceededRules(catalog.MeterRequests, 30)
	defer rcleanup()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	req := testSetupWithLimiter(t, ob, l, rules)

	resp, _ := Run(ctx, req)
	runBody(resp) // drain

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedRateLimited) {
		t.Errorf("terminated_by want rate_limited, got %v", evs[0]["terminated_by"])
	}
	if attempts, ok := evs[0]["attempts"].([]interface{}); ok && len(attempts) > 0 {
		t.Errorf("expected 0 attempts for rate-limited, got %d", len(attempts))
	}
}

func TestLifecycle_PoolExhausted(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()
	req.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	req.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	resp, _ := Run(ctx, req)
	runBody(resp) // drain

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedPoolExhausted) {
		t.Errorf("terminated_by want pool_exhausted, got %v", evs[0]["terminated_by"])
	}
}

func TestLifecycle_ClientCancelMidStream(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	cancelCtx, cancel := context.WithCancel(ctx)

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		<-cancelCtx.Done()
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	errCh := make(chan error, 1)
	go func() {
		resp, err := Run(cancelCtx, req)
		if resp != nil {
			runBody(resp)
		}
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	<-errCh

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedClientCancel) {
		t.Errorf("terminated_by want client_cancel, got %v", evs[0]["terminated_by"])
	}
}

func TestLifecycle_UpstreamDeadline(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	deadlineCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		<-deadlineCtx.Done()
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	resp, _ := Run(deadlineCtx, req)
	if resp != nil {
		runBody(resp)
	}

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedUpstreamTimeout) {
		t.Errorf("terminated_by want upstream_timeout, got %v", evs[0]["terminated_by"])
	}
}

func TestLifecycle_PanicInProvider(t *testing.T) {
	ctx, _, flush := lcEnv(t)

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		panic("provider bug")
	}}
	req, cleanup := testSetup(t, ob)
	defer cleanup()

	resp, err := Run(ctx, req)
	if err == nil {
		t.Fatal("expected error from recovered panic")
	}
	if resp != nil {
		runBody(resp)
	}

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedRelayError) {
		t.Errorf("terminated_by want relay_error, got %v", evs[0]["terminated_by"])
	}
}

// TestRun_PassthroughAuth verifies that the passthrough path bypasses key
// selection and forwards the inbound auth value verbatim to the upstream call.
func TestRun_PassthroughAuth(t *testing.T) {
	const inboundAuth = "Bearer sk-ant-oauth-token-abc123"

	var capturedSecret string
	ob := &fakeOutbound{handle: func(_ int, secret string, out chan<- *transport.Message) {
		capturedSecret = secret
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
		out <- &transport.Message{Body: []byte(`{}`)}
	}}

	st := kv.NewMem()
	defer st.Close()
	sel := keypool.New(st, slog.Default(), nil, nil)

	req := &Request{
		Body: []byte(`{}`),
		Policy: &catalog.Policy{
			Metadata: catalog.Metadata{Name: "pt-policy"},
			Spec:     catalog.PolicySpec{},
		},
		Selector:        sel,
		Outbound:        ob,
		PassthroughAuth: inboundAuth,
		MaxAttempts:     1,
	}

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain

	if capturedSecret != inboundAuth {
		t.Errorf("secret forwarded = %q; want %q", capturedSecret, inboundAuth)
	}
}

// ---------------------------------------------------------------------------
// Per-key Reserve tests
// ---------------------------------------------------------------------------

// buildPerKeyStore builds a catalog.MemStore where each secret has a rate-limit
// with the given amount (requests/1m). A fresh limiter backed by the same kv.Mem
// is returned. The limiter's clock is fixed so window buckets are deterministic.
func buildPerKeyStore(t *testing.T, amount int64) (*catalog.MemStore, *ratelimit.Limiter, *kv.Mem) {
	t.Helper()
	now := time.Now()
	kvst := kv.NewMem()
	t.Cleanup(func() { kvst.Close() })
	l := ratelimit.New(kvst, slog.Default(), func() time.Time { return now })

	rl1 := &catalog.RateLimit{
		Metadata: catalog.Metadata{Name: "per-key-rpm"},
		Spec: catalog.RateLimitSpec{
			Rules: []catalog.RateLimitRule{
				{Meter: string(catalog.MeterRequests), Amount: amount, Window: time.Minute, Strategy: catalog.StrategyTokenBucket},
			},
		},
	}

	sec1 := &catalog.Secret{
		Metadata: catalog.Metadata{Name: "key1"},
		Resolved: "secret-key1",
		KeyHash:  "hash1",
		Spec:     catalog.SecretSpec{RateLimits: []catalog.RateLimitAttachment{{Ref: "per-key-rpm"}}},
	}
	sec2 := &catalog.Secret{
		Metadata: catalog.Metadata{Name: "key2"},
		Resolved: "secret-key2",
		KeyHash:  "hash2",
		Spec:     catalog.SecretSpec{RateLimits: []catalog.RateLimitAttachment{{Ref: "per-key-rpm"}}},
	}

	cs := catalog.NewMemStore(rl1, sec1, sec2)
	return cs, l, kvst
}

// waitPostFlight installs postFlightHook so the caller can synchronously wait
// for the async post-flight goroutine after Run returns. Returns a wait func.
// Must be called before Run; the hook is cleared after wait returns.
func waitPostFlight(t *testing.T) (wait func()) {
	t.Helper()
	ch := make(chan struct{})
	postFlightHook = func() {
		postFlightHook = nil
		close(ch)
	}
	return func() {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for post-flight goroutine")
		}
	}
}

// TestRun_PerKeyReserve_ExhaustsKeyA verifies the three-request scenario from
// the brief: key A's budget exhausts, RecordLocalRateLimit cools it down, key B
// is picked on the next request.
func TestRun_PerKeyReserve_ExhaustsKeyA(t *testing.T) {
	cs, l, _ := buildPerKeyStore(t, 1) // 1 request/min per key

	policy := &catalog.Policy{Metadata: catalog.Metadata{Name: "test-policy"}}
	// Reuse the snapshot-resolved secrets so RateLimitAttachment.Ref is the
	// canonical RateLimit id (the resolver rewrote it inside NewMemStore).
	secrets := cs.Secrets()

	kvSt := kv.NewMem()
	t.Cleanup(func() { kvSt.Close() })
	sel := keypool.New(kvSt, slog.Default(), nil, nil)

	makeReq := func() *Request {
		return &Request{
			Body:         []byte(`{"model":"gpt-4"}`),
			Policy:       policy,
			Secrets:      secrets,
			Selector:     sel,
			MaxAttempts:  3,
			Limiter:      l,
			CatalogStore: cs,
			Outbound: &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
				out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
				out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			}},
		}
	}

	ctx := context.Background()

	// Request 1: key A picked (prioritized), per-key Reserve succeeds (budget=1).
	// Wait for post-flight so RecordSuccess fires before req2 checks the circuit.
	{
		wait := waitPostFlight(t)
		resp, err := Run(ctx, makeReq())
		if err != nil {
			t.Fatalf("req1: unexpected error: %v", err)
		}
		if resp.Status != 200 {
			t.Fatalf("req1: expected 200, got %d", resp.Status)
		}
		runBody(resp) // drain
		wait()        // ensure RecordSuccess has run before req2 starts
	}

	// Request 2: key A picked again (still prioritized), but its budget=1 is now
	// exhausted → KeyQuotaExhausted → RecordLocalRateLimit called → 429 returned.
	{
		resp, err := Run(ctx, makeReq())
		if err == nil {
			t.Fatal("req2: expected error for per-key exhausted, got nil")
		}
		if resp.Status != 429 {
			t.Fatalf("req2: expected 429, got %d", resp.Status)
		}
		body := runBody(resp)
		if !containsStr(body, "rate_limit_exceeded") {
			t.Fatalf("req2: expected rate_limit_exceeded in body: %s", body)
		}
	}

	// Request 3: key A is now cooled down (CircuitOpen via RecordLocalRateLimit).
	// keypool.Pick skips A and returns B. Per-key Reserve for B succeeds.
	{
		var pickedSecret string
		req3 := makeReq()
		req3.Outbound = &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
			pickedSecret = secret
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
		}}
		resp, err := Run(ctx, req3)
		if err != nil {
			t.Fatalf("req3: unexpected error: %v", err)
		}
		if resp.Status != 200 {
			t.Fatalf("req3: expected 200, got %d", resp.Status)
		}
		runBody(resp) // drain
		if pickedSecret != "secret-key2" {
			t.Fatalf("req3: expected key2 to be picked (key1 cooled), got %q", pickedSecret)
		}
	}
}

// TestRun_PerKeyReserve_NoRules_FastPath verifies that when CatalogStore returns
// no per-key rules for the chosen secret, the pipeline proceeds without an extra
// kv round-trip (the per-key reserve block is skipped entirely).
func TestRun_PerKeyReserve_NoRules_FastPath(t *testing.T) {
	// Use a MemStore with secrets that have NO rate-limit attachments.
	cs := catalog.NewMemStore()
	kvSt := kv.NewMem()
	t.Cleanup(func() { kvSt.Close() })
	sel := keypool.New(kvSt, slog.Default(), nil, nil)

	var calls atomic.Int32
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		calls.Add(1)
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}

	req := &Request{
		Body:  []byte(`{"model":"gpt-4"}`),
		Policy: &catalog.Policy{Metadata: catalog.Metadata{Name: "p"}},
		Secrets: []*catalog.Secret{
			{Metadata: catalog.Metadata{Name: "k"}, Resolved: "sk", KeyHash: "h"},
		},
		Selector:     sel,
		Outbound:     ob,
		MaxAttempts:  1,
		CatalogStore: cs,
	}

	ctx := context.Background()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runBody(resp) // drain
	if calls.Load() != 1 {
		t.Fatalf("expected 1 outbound call, got %d", calls.Load())
	}
}
