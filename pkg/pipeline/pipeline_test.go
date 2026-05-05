package pipeline

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/keypool"
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
		t.Fatalf("expected 1 pool_exhausted message, got %d", len(msgs))
	}
	if msgs[0].Headers["X-Relay-Status"] != "502" {
		t.Fatalf("expected 502 status in pool_exhausted message")
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
		t.Fatalf("expected 1 pool_exhausted message, got %d", len(msgs))
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
