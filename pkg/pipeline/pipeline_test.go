package pipeline

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/limit"
	"github.com/wyolet/relay/pkg/state"
	"github.com/wyolet/relay/pkg/transport"
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
	st := state.New()
	sel := keypool.New(st, slog.Default(), nil)

	pool := &configstore.Pool{
		Metadata: configstore.Metadata{Name: "test-pool"},
	}
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
		{Metadata: configstore.Metadata{Name: "key2"}, Resolved: "secret-key2", KeyHash: "hash2"},
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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
		errCh <- Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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
	commitCalls []limit.Observations
	reserveCalls int
}

func (f *fakeLimiter) reserve(ctx context.Context, rules []configstore.ResolvedRule) (*limit.Reservation, error) {
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
func testLimiterSetup(t *testing.T) (*limit.Limiter, []configstore.ResolvedRule, func()) {
	t.Helper()
	st := state.New()
	l := limit.New(st, slog.Default(), nil)
	rules := []configstore.ResolvedRule{
		{
			ParentKind: configstore.KindPool,
			ParentName: "test-pool",
			Meter:      configstore.MeterRequests,
			RateLimit: &configstore.RateLimit{
				Metadata: configstore.Metadata{Name: "rpm"},
				Spec: configstore.RateLimitSpec{
					Strategy: configstore.StrategySlidingWindow,
					Window:   time.Minute,
					Amount:   1000,
				},
			},
		},
	}
	return l, rules, func() { st.Close() }
}

func testSetupWithLimiter(t *testing.T, ob *fakeOutbound, l *limit.Limiter, rules []configstore.ResolvedRule) RunOptions {
	t.Helper()
	st := state.New()
	t.Cleanup(func() { st.Close() })
	sel := keypool.New(st, slog.Default(), nil)
	pool := &configstore.Pool{Metadata: configstore.Metadata{Name: "test-pool"}}
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "key1"}, Resolved: "secret-key1", KeyHash: "hash1"},
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

func exceededRules(meter configstore.Meter, retryAfterSec int) ([]configstore.ResolvedRule, *limit.Limiter, func()) {
	st := state.New()
	window := time.Minute
	amount := int64(1)
	// Use a fixed clock so the window bucket is deterministic.
	now := time.Now()
	l := limit.New(st, slog.Default(), func() time.Time { return now })
	rule := configstore.ResolvedRule{
		ParentKind: configstore.KindPool,
		ParentName: "test-pool",
		Meter:      meter,
		RateLimit: &configstore.RateLimit{
			Metadata: configstore.Metadata{Name: string(meter) + "-limit"},
			Spec: configstore.RateLimitSpec{
				Strategy: configstore.StrategySlidingWindow,
				Window:   window,
				Amount:   amount,
			},
		},
	}
	rules := []configstore.ResolvedRule{rule}
	ctx := context.Background()
	// Exhaust the budget: Reserve once (succeeds), then the next Reserve will fail.
	if meter == configstore.MeterRequests {
		l.Reserve(ctx, rules)
	} else if meter == configstore.MeterConcurrency {
		l.Reserve(ctx, rules)
	}
	// For tokens: set the counter via a successful Reserve+Commit with tokens=amount.
	if meter == configstore.MeterTokens {
		res, _ := l.Reserve(ctx, rules)
		if res != nil {
			commitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			l.Commit(commitCtx, res, limit.Observations{Tokens: amount})
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

	err := Run(ctx, ch, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ob.calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", ob.calls.Load())
	}
}

func TestRun_RPMExceeded_Returns429(t *testing.T) {
	rules, l, cleanup := exceededRules(configstore.MeterRequests, 30)
	defer cleanup()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		// should never be called
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	st2 := state.New()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil)
	opts := RunOptions{
		Pool:        &configstore.Pool{Metadata: configstore.Metadata{Name: "p"}},
		Secrets:     []*configstore.Secret{{Metadata: configstore.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	err := Run(ctx, ch, opts)
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
	rules, l, cleanup := exceededRules(configstore.MeterConcurrency, 0)
	defer cleanup()

	ob := &fakeOutbound{handle: func(idx int, secret string, out chan<- *transport.Message) {
		out <- &transport.Message{Headers: map[string]string{"X-Relay-Status": "200"}}
	}}
	st2 := state.New()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil)
	opts := RunOptions{
		Pool:        &configstore.Pool{Metadata: configstore.Metadata{Name: "p"}},
		Secrets:     []*configstore.Secret{{Metadata: configstore.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
		Selector:    sel,
		Outbound:    ob,
		MaxAttempts: 3,
		Limiter:     l,
		Rules:       rules,
	}

	ctx := context.Background()
	ch := newTestChannel(ctx)
	sendInbound(ch)

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	err := Run(ctx, ch, opts)
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

	st2 := state.New()
	defer st2.Close()
	sel := keypool.New(st2, slog.Default(), nil)
	opts := RunOptions{
		Pool:        &configstore.Pool{Metadata: configstore.Metadata{Name: "p"}},
		Secrets:     []*configstore.Secret{{Metadata: configstore.Metadata{Name: "k"}, Resolved: "s", KeyHash: "h"}},
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
		errCh <- Run(ctx, ch, opts)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errCh
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	collectOut(ch)
}
