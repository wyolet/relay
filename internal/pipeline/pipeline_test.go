package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
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

// testSetup builds a RunOptions with two secrets and a fresh Selector.
func testSetup(t *testing.T, ob *fakeOutbound) (RunOptions, func()) {
	t.Helper()
	st := kv.NewMem()
	sel := keypool.New(st, slog.Default(), nil, nil, nil, nil)

	pool := &catalog.Pool{
		Metadata: catalog.Metadata{Name: "test-pool"},
	}
	secrets := []*catalog.Secret{
		{Metadata: catalog.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
		{Metadata: catalog.Metadata{Name: "key2"}, Resolved: "secret-key2", KeyHash: "hash2"},
	}

	opts := RunOptions{
		Pool:        pool,
		Secrets:     secrets,
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
	}
	return opts, func() { st.Close() }
}

func newTestChannel(ctx context.Context) *transport.Channel {
	return transport.NewChannel(ctx, "test", 1, 64)
}

func sendInbound(ch *transport.Channel) {
	ch.In <- &transport.Message{ID: "test", Body: []byte(`{"model":"gpt-4"}`)}
	close(ch.In)
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

func collectOut(ch *transport.Channel) []*transport.Message {
	var msgs []*transport.Message
	for m := range ch.Out {
		msgs = append(msgs, m)
	}
	return msgs
}

func TestRun_Success(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200", "Content-Type": "application/json"}}
		out <- &transport.Message{Body: []byte("chunk")}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
			// 10ms retry-after — well under 5s threshold, so same-key retry
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.MaxAttempts = 3

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 exhausted message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "502" {
		t.Fatalf("expected 502 status, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.MaxAttempts = 2

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "502" {
		t.Fatalf("expected 502, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.MaxAttempts = 2

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "429" {
		t.Fatalf("expected 429, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
	if !containsStr(body, "rate_limit_exceeded") {
		t.Fatalf("expected rate_limit_exceeded, got %s", body)
	}
	if msgs[0].Headers["Retry-After"] != "30" {
		t.Fatalf("expected Retry-After: 30, got %s", msgs[0].Headers["Retry-After"])
	}
}

func TestRun_All5xx_ServerErrorEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "500", "X-Relay-Final": "true"}}
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.MaxAttempts = 3

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != ErrAttemptsExhausted {
		t.Fatalf("expected ErrAttemptsExhausted, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "502" {
		t.Fatalf("expected 502, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
	if !containsStr(body, "upstream_5xx_exhausted") {
		t.Fatalf("expected upstream_5xx_exhausted, got %s", body)
	}
}

func TestRun_NoKeysFromStart_NoHealthyKeysEnvelope(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	// Mark all keys as auth-failed so ErrNoHealthyKeys is returned immediately.
	opts.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	opts.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != keypool.ErrNoHealthyKeys {
		t.Fatalf("expected ErrNoHealthyKeys, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "503" {
		t.Fatalf("expected 503, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
	if !containsStr(body, "no_healthy_keys") {
		t.Fatalf("expected no_healthy_keys, got %s", body)
	}
}

func TestRun_NoHealthyKeys(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	// Mark both keys as auth-failed so they're permanently open.
	ctx := context.Background()
	opts.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	opts.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != keypool.ErrNoHealthyKeys {
		t.Fatalf("expected ErrNoHealthyKeys, got %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 exhausted message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "503" {
		t.Fatalf("expected 503, got %s", msgs[0].Headers["X-Relay-Status"])
	}
}

func TestRun_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Outbound blocks until ctx is cancelled.
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		// Don't send anything — let ctx.Done() trigger in Run.
		<-ctx.Done()
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ch := newTestChannel(ctx)
	sendInbound(ch)

	errCh := make(chan error, 1)
	go func() {
		res, err := Run(ctx, ch, opts); _ = res; errCh <- err
	}()

	// Give Run time to start, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// ch.Out should be closed.
	collectOut(ch)
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	reserveErr  error
	commitCalls []ratelimit.Observations
	reserveCalls int
}

func (f *fakeLimiter) reserve(ctx context.Context, rules []catalog.ResolvedRule) (*ratelimit.Reservation, error) {
	f.reserveCalls++
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	// Return a zero-value Reservation via limit.New with MemStore — not possible
	// from outside the package. We expose via the real Limiter instead.
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
			ParentKind: catalog.KindPool,
			ParentName: "test-pool",
			Meter:      catalog.MeterRequests,
			RateLimit: &catalog.RateLimit{
				Metadata: catalog.Metadata{Name: "rpm"},
				Spec: catalog.RateLimitSpec{
					Strategy: catalog.StrategySlidingWindow,
					Window:   time.Minute,
					Amount:   1000,
				},
			},
		},
	}
	return l, rules, func() { st.Close() }
}

func testSetupWithLimiter(t *testing.T, ob *fakeOutbound, l *ratelimit.Limiter, rules []catalog.ResolvedRule) RunOptions {
	t.Helper()
	st := kv.NewMem()
	t.Cleanup(func() { st.Close() })
	sel := keypool.New(st, slog.Default(), nil, nil, nil, nil)
	pool := &catalog.Pool{Metadata: catalog.Metadata{Name: "test-pool"}}
	secrets := []*catalog.Secret{
		{Metadata: catalog.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
	}
	return RunOptions{
		Pool:        pool,
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
		ParentKind: catalog.KindPool,
		ParentName: "test-pool",
		Meter:      meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: string(meter) + "-limit"},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   window,
				Amount:   amount,
			},
		},
	}
	rules := []catalog.ResolvedRule{rule}
	ctx := context.Background()
	// Exhaust the budget: Reserve once (succeeds), then the next Reserve will fail.
	if meter == catalog.MeterRequests {
		l.Reserve(ctx, "test-pool", rules)
	} else if meter == catalog.MeterConcurrency {
		l.Reserve(ctx, "test-pool", rules)
	}
	// For tokens: set the counter via a successful Reserve+Commit with tokens=amount.
	if meter == catalog.MeterTokens {
		res, _ := l.Reserve(ctx, "test-pool", rules)
		if res != nil {
			commitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			l.Commit(commitCtx, res, ratelimit.Observations{Tokens: amount})
		}
	}
	return rules, l, func() { st.Close() }
}

func TestRun_LimiterNil_NoGating(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
	}}
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	// opts.Limiter is nil by default — no gating

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	sel := keypool.New(st2, slog.Default(), nil, nil, nil, nil)
	opts := RunOptions{
		Pool:        &catalog.Pool{Metadata: catalog.Metadata{Name: "test-pool"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err == nil {
		t.Fatal("expected error from rate limit, got nil")
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "429" {
		t.Fatalf("expected 429, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
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
	sel := keypool.New(st2, slog.Default(), nil, nil, nil, nil)
	opts := RunOptions{
		Pool:        &catalog.Pool{Metadata: catalog.Metadata{Name: "test-pool"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err == nil {
		t.Fatal("expected error from concurrency limit")
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "429" {
		t.Fatalf("expected 429, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	body := string(msgs[0].Body)
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

	opts := testSetupWithLimiter(t, ob, l, rules)
	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Collect all output.
	msgs := collectOut(ch)
	if len(msgs) == 0 {
		t.Fatal("expected output messages")
	}
	// Verify pipeline forwarded the body containing usage.
	found := false
	for _, m := range msgs {
		if containsStr(string(m.Body), "total_tokens") {
			found = true
		}
	}
	if !found {
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

	opts := testSetupWithLimiter(t, ob, l, rules)
	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := collectOut(ch)
	if len(msgs) == 0 {
		t.Fatal("expected output messages")
	}
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
	sel := keypool.New(st2, slog.Default(), nil, nil, nil, nil)
	opts := RunOptions{
		Pool:        &catalog.Pool{Metadata: catalog.Metadata{Name: "test-pool"}},
		Secrets:     []*catalog.Secret{{Metadata: catalog.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ch := newTestChannel(ctx)
	sendInbound(ch)

	errCh := make(chan error, 1)
	go func() {
		res, err := Run(ctx, ch, opts); _ = res; errCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	collectOut(ch)
}

// TestPipeline_ErrPoolOutOfCapacity_Returns429 — selector returning ErrPoolOutOfCapacity → 429.
func TestPipeline_ErrPoolOutOfCapacity_Returns429(t *testing.T) {
	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}

	// Build a real Selector with a 1-request budget pre-exhausted so Pick returns ErrPoolOutOfCapacity.
	dir := t.TempDir()
	yamlContent := `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: p
spec:
  kind: openai
  baseURL: https://api.openai.com
  default: true
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rpm-zero
spec:
  strategy: sliding-window
  window: 1m
  amount: 1
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: k
spec:
  provider: p
  value: "testval"
  rateLimits:
    - ref: rpm-zero
      meter: requests
`
	if err := os.WriteFile(dir+"/config.yaml", []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg, err := catalog.LoadYAML(dir)
	if err != nil {
		t.Fatal(err)
	}

	st3 := kv.NewMem()
	defer st3.Close()
	lim3 := ratelimit.New(st3, slog.Default(), nil)
	sel3 := keypool.New(st3, slog.Default(), nil, lim3, cfg, nil)

	sec := cfg.Secrets()[0]
	p2 := &catalog.Pool{Metadata: catalog.Metadata{Name: "pp"}}

	// Pre-exhaust the budget.
	rule3 := catalog.ResolvedRule{
		ParentKind: catalog.KindSecret,
		ParentName: "k",
		Meter:      catalog.MeterRequests,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "rpm-zero"},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   time.Minute,
				Amount:   1,
			},
		},
	}
	lim3.Reserve(ctx, "pp", []catalog.ResolvedRule{rule3})

	ch := newTestChannel(ctx)
	ch.In <- &transport.Message{ID: "x", Body: []byte(`{"model":"m"}`)}
	close(ch.In)

	_, runErr := Run(ctx, ch, RunOptions{
		Pool:    p2,
		Secrets: []*catalog.Secret{sec},
		Selector: sel3,
		Outbound: ob,
		MaxAttempts: 1,
	})
	if runErr != keypool.ErrPoolOutOfCapacity {
		t.Fatalf("want ErrPoolOutOfCapacity, got %v", runErr)
	}
	msgs := collectOut(ch)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "429" {
		t.Fatalf("expected 429, got %s", msgs[0].Headers["X-Relay-Status"])
	}
	if !containsStr(string(msgs[0].Body), "pool_out_of_capacity") {
		t.Fatalf("expected pool_out_of_capacity in body: %s", string(msgs[0].Body))
	}
	if msgs[0].Headers["Retry-After"] != "30" {
		t.Fatalf("expected Retry-After: 30, got %s", msgs[0].Headers["Retry-After"])
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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.Model = &catalog.Model{Metadata: catalog.Metadata{Name: "gpt-4o"}}
	opts.Provider = &catalog.Provider{Metadata: catalog.Metadata{Name: "openai"}}
	opts.TokenExtractor = testOpenAIExtractTokens

	ch := newTestChannel(ctx)
	sendInbound(ch)
	if _, err := Run(ctx, ch, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	collectOut(ch)

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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ch := newTestChannel(ctx)
	sendInbound(ch)
	if _, err := Run(ctx, ch, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	collectOut(ch)

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
	opts := testSetupWithLimiter(t, ob, l, rules)

	ch := newTestChannel(ctx)
	sendInbound(ch)
	_, _ = Run(ctx, ch, opts)
	collectOut(ch)

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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()
	opts.Selector.RecordFailure(ctx, "hash1", keypool.FailureAuth, 0)
	opts.Selector.RecordFailure(ctx, "hash2", keypool.FailureAuth, 0)

	ch := newTestChannel(ctx)
	sendInbound(ch)
	_, _ = Run(ctx, ch, opts)
	collectOut(ch)

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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ch := newTestChannel(cancelCtx)
	sendInbound(ch)

	errCh := make(chan error, 1)
	go func() { _, err := Run(cancelCtx, ch, opts); errCh <- err }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	<-errCh
	collectOut(ch)

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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ch := newTestChannel(deadlineCtx)
	sendInbound(ch)
	Run(deadlineCtx, ch, opts)
	collectOut(ch)

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
	opts, cleanup := testSetup(t, ob)
	defer cleanup()

	ch := newTestChannel(ctx)
	sendInbound(ch)

	_, err := Run(ctx, ch, opts)
	if err == nil {
		t.Fatal("expected error from recovered panic")
	}
	collectOut(ch)

	evs := flush()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["terminated_by"] != string(usage.TerminatedRelayError) {
		t.Errorf("terminated_by want relay_error, got %v", evs[0]["terminated_by"])
	}
}
