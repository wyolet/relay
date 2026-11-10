package settle_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/hybrid"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/settle"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

var errInjected = errors.New("injected command failure")

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// failingCommands fails, after the batch has run, every batched command that touches a key containing marker, as a command's own error reply would.
type failingCommands struct {
	kvtest.ScriptStore
	marker string
}

func (f *failingCommands) ExecBatch(ctx context.Context, ops []kv.BatchOp) {
	execBatch(ctx, f.ScriptStore, ops)
	for _, op := range ops {
		for _, k := range op.Script.Keys {
			if strings.Contains(k, f.marker) {
				op.Result.Err = errInjected
				break
			}
		}
	}
}

func execBatch(ctx context.Context, s kvtest.ScriptStore, ops []kv.BatchOp) {
	if b, ok := s.(kv.Batcher); ok {
		b.ExecBatch(ctx, ops)
		return
	}
	kv.ExecInOrder(ctx, s, ops)
}

// unavailableOnce fails the next batch after arm in transport, as a stalled or dropped connection would: every command reports the store unavailable. With ran the commands run first and only their replies are lost.
type unavailableOnce struct {
	kvtest.ScriptStore
	ran   bool
	armed atomic.Bool
}

func (u *unavailableOnce) arm() { u.armed.Store(true) }

func (u *unavailableOnce) ExecBatch(ctx context.Context, ops []kv.BatchOp) {
	if !u.armed.CompareAndSwap(true, false) {
		execBatch(ctx, u.ScriptStore, ops)
		return
	}
	if u.ran {
		execBatch(ctx, u.ScriptStore, ops)
	}
	for _, op := range ops {
		op.Result.Value, op.Result.Err = nil, fmt.Errorf("%w: injected reply loss", kv.ErrUnavailable)
	}
}

var testRateLimit = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-1"}}

func counter(rule ratelimit.Rule, team string) limits.Counter {
	return limits.CallerRule(limits.RungTeam, testRateLimit, rule, "pol-1", "account:"+team, "account:"+team)
}

func leases(n int64) ratelimit.Rule {
	return ratelimit.Rule{Meter: ratelimit.MeterConcurrency, Strategy: ratelimit.StrategySlidingWindow, Amount: n, Window: ratelimit.Window(time.Minute)}
}

// fixture counts on a Lua limiter over a recording store; a released lease is what shows a settle landed.
type fixture struct {
	lim   limits.Limiter
	store *kvtest.Recording
	rule  ratelimit.Rule
}

func newFixture(t *testing.T, s kvtest.ScriptStore, rule ratelimit.Rule) fixture {
	t.Helper()
	rec := kvtest.NewRecording(s)
	return fixture{lim: lua.New(pkgratelimit.New(rec, discard(), nil)), store: rec, rule: rule}
}

func (f fixture) reserve(t *testing.T, team string, n int) []limits.Reservation {
	t.Helper()
	out := make([]limits.Reservation, n)
	for i := range out {
		res, err := f.lim.Admit(context.Background(), []limits.Counter{counter(f.rule, team)}, limits.Usage{})
		if err != nil {
			t.Fatalf("reserve %d for %s: %v", i, team, err)
		}
		out[i] = res
	}
	return out
}

func (f fixture) assertFull(t *testing.T, team string) {
	t.Helper()
	var r *limits.Refused
	if _, err := f.lim.Admit(context.Background(), []limits.Counter{counter(f.rule, team)}, limits.Usage{}); !errors.As(err, &r) {
		t.Fatalf("reserve for %s = %v, want a refusal while its leases are held", team, err)
	}
}

func (f fixture) queue(t *testing.T, cfg settle.Config) *settle.Queue {
	t.Helper()
	q := settle.New(f.store, cfg, discard())
	t.Cleanup(q.Close)
	return q
}

func settleOn(ctx context.Context, q *settle.Queue, res limits.Reservation, u limits.Usage) {
	b := q.NewBatch()
	q.Settle(ctx, b, res.(limits.BatchSettler).PrepareSettle(b, u))
}

func settleAll(q *settle.Queue, res []limits.Reservation) {
	var wg sync.WaitGroup
	for _, r := range res {
		wg.Go(func() { settleOn(context.Background(), q, r, limits.Usage{}) })
	}
	wg.Wait()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func runQueueSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	t.Run("full_batch_is_one_round_trip", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(4))
		q := f.queue(t, settle.Config{BatchSize: 4, MaxDelay: time.Hour, QueueSize: 16})
		q.Start()
		res := f.reserve(t, "team-1", 4)
		f.assertFull(t, "team-1")
		trips := f.store.RoundTrips()
		settleAll(q, res)
		if got, batches := f.store.RoundTrips()-trips, f.store.Batches(); got != 1 || batches[len(batches)-1] != 4 {
			t.Fatalf("round trips = %d, batches = %v; want one batch of the 4 commits", got, batches)
		}
		f.reserve(t, "team-1", 4)
	})

	t.Run("partial_batch_goes_after_the_delay", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(1))
		q := f.queue(t, settle.Config{BatchSize: 64, MaxDelay: 5 * time.Millisecond, QueueSize: 16})
		q.Start()
		res := f.reserve(t, "team-1", 1)
		settleOn(context.Background(), q, res[0], limits.Usage{})
		f.reserve(t, "team-1", 1)
	})

	t.Run("nothing_to_write_does_not_wait", func(t *testing.T) {
		f := newFixture(t, newStore(t), ratelimit.Rule{Meter: ratelimit.MeterRequests, Strategy: ratelimit.StrategyFixedWindow, Amount: 10, Window: ratelimit.Window(time.Minute)})
		// Never started: a settle that waited for a writer would hang.
		q := f.queue(t, settle.Config{BatchSize: 64, MaxDelay: time.Hour, QueueSize: 16})
		res := f.reserve(t, "team-1", 1)
		trips := f.store.RoundTrips()
		settleOn(context.Background(), q, res[0], limits.Usage{})
		if got := f.store.RoundTrips() - trips; got != 0 {
			t.Fatalf("round trips = %d for a settle with nothing to write, want 0", got)
		}
	})

	t.Run("full_queue_settles_directly", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(2))
		q := f.queue(t, settle.Config{BatchSize: 8, QueueSize: 1})
		res := f.reserve(t, "team-1", 2)
		queued := make(chan struct{})
		go func() {
			settleOn(context.Background(), q, res[0], limits.Usage{})
			close(queued)
		}()
		waitFor(t, "the first settle to fill the queue", func() bool { return q.Queued() == 1 })

		full := testutil.ToFloat64(metrics.SettleQueueFull)
		trips := f.store.RoundTrips()
		settleOn(context.Background(), q, res[1], limits.Usage{})
		if got := testutil.ToFloat64(metrics.SettleQueueFull) - full; got != 1 {
			t.Fatalf("settle_queue_full_total moved by %v, want 1", got)
		}
		if got := f.store.RoundTrips() - trips; got != 1 {
			t.Fatalf("round trips = %d, want the direct settle's one", got)
		}
		// The direct settle released its lease; the queued one still holds its own.
		f.reserve(t, "team-1", 1)
		f.assertFull(t, "team-1")

		q.Start()
		<-queued
		f.reserve(t, "team-1", 1)
	})

	t.Run("close_drains_the_queue", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(3))
		q := f.queue(t, settle.Config{BatchSize: 64, MaxDelay: time.Hour, QueueSize: 8})
		res := f.reserve(t, "team-1", 3)
		var wg sync.WaitGroup
		for _, r := range res {
			wg.Go(func() { settleOn(context.Background(), q, r, limits.Usage{}) })
		}
		waitFor(t, "the settles to queue", func() bool { return q.Queued() == len(res) })

		q.Start()
		// The batch waits on an hour's delay: only the close sends it.
		q.Close()
		wg.Wait()
		res = f.reserve(t, "team-1", 3)

		// A closed queue turns settles direct, which is not a full queue.
		full := testutil.ToFloat64(metrics.SettleQueueFull)
		settleOn(context.Background(), q, res[0], limits.Usage{})
		if got := testutil.ToFloat64(metrics.SettleQueueFull) - full; got != 0 {
			t.Fatalf("settle_queue_full_total moved by %v after close, want 0", got)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("failed_command_fails_only_its_settle", func(t *testing.T) {
		f := newFixture(t, &failingCommands{ScriptStore: newStore(t), marker: "team-bad"}, leases(1))
		q := f.queue(t, settle.Config{BatchSize: 2, MaxDelay: time.Hour, QueueSize: 16})
		q.Start()
		good, bad := f.reserve(t, "team-good", 1), f.reserve(t, "team-bad", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		settleAll(q, append(good, bad...))
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 0 {
			t.Fatalf("settle_retries_total moved by %v for an error reply, want 0", got)
		}
		if batches := f.store.Batches(); batches[len(batches)-1] != 2 {
			t.Fatalf("batches = %v, want both settles in one", batches)
		}
		f.reserve(t, "team-good", 1)
	})

	t.Run("unsent_batch_is_resent", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t)}
		f := newFixture(t, store, leases(1))
		q := f.queue(t, settle.Config{BatchSize: 8, QueueSize: 16})
		q.Start()
		res := f.reserve(t, "team-1", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		settleOn(context.Background(), q, res[0], limits.Usage{})
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("settle_sent_alone_is_resent", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t)}
		f := newFixture(t, store, leases(1))
		q := f.queue(t, settle.Config{BatchSize: 8, QueueSize: 16})
		q.Close() // a closed queue sends each settle alone
		res := f.reserve(t, "team-1", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the caller's cancellation does not reach the send
		settleOn(ctx, q, res[0], limits.Usage{})
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("resent_commit_that_ran_counts_once", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t), ran: true}
		f := newFixture(t, store, ratelimit.Rule{Meter: ratelimit.MeterTokens, Strategy: ratelimit.StrategyFixedWindow, Amount: 100, Window: ratelimit.Window(time.Hour)})
		q := f.queue(t, settle.Config{BatchSize: 8, QueueSize: 16})
		q.Start()
		u := limits.Usage{Tokens: usage.Tokens{"input": 40, "output": 20}}
		res := f.reserve(t, "team-1", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		settleOn(context.Background(), q, res[0], u)
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		// 60 of 100 tokens: counted twice, the meter would already be spent.
		res = f.reserve(t, "team-1", 1)
		settleOn(context.Background(), q, res[0], u)
		f.assertFull(t, "team-1")
	})

	t.Run("caller_deadline_does_not_lose_the_settle", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(1))
		q := f.queue(t, settle.Config{BatchSize: 8, QueueSize: 4})
		res := f.reserve(t, "team-1", 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		settleOn(ctx, q, res[0], limits.Usage{})
		q.Start()
		waitFor(t, "the abandoned settle to land", func() bool {
			r, err := f.lim.Admit(context.Background(), []limits.Counter{counter(f.rule, "team-1")}, limits.Usage{})
			if err == nil {
				r.Release()
			}
			return err == nil
		})
	})

	t.Run("hybrid_exact_counters", func(t *testing.T) {
		f := newFixture(t, newStore(t), leases(1))
		f.lim = hybrid.New(f.lim.(*lua.Limiter), f.store, f.store, time.Hour)
		q := f.queue(t, settle.Config{BatchSize: 8, MaxDelay: time.Millisecond, QueueSize: 16})
		q.Start()
		res := f.reserve(t, "team-1", 1)
		f.assertFull(t, "team-1")
		trips := f.store.RoundTrips()
		settleOn(context.Background(), q, res[0], limits.Usage{})
		if got := f.store.RoundTrips() - trips; got != 1 {
			t.Fatalf("round trips = %d, want the shared batch's one", got)
		}
		f.reserve(t, "team-1", 1)
	})
}

func memStore(t *testing.T) kvtest.ScriptStore {
	m := kv.NewMem()
	t.Cleanup(func() { _ = m.Close() })
	pkgratelimit.RegisterScripts(m)
	return m
}

func TestQueue_Mem(t *testing.T) { runQueueSuite(t, memStore) }

// Close waits as long as the pending settles need at worst: one round of the writers per four full batches, each round a delay, a batch and a resend.
func TestQueue_DrainTimeout(t *testing.T) {
	q := settle.New(kv.NewMem(), settle.Config{BatchSize: 64, MaxDelay: 2 * time.Millisecond, QueueSize: 4096}, discard())
	round := 2*time.Millisecond + 10*time.Second
	for _, tc := range []struct {
		pending int64
		want    time.Duration
	}{{0, round}, {1, round}, {256, round}, {257, 2 * round}, {4096, 16 * round}} {
		if got := q.DrainTimeout(tc.pending); got != tc.want {
			t.Errorf("DrainTimeout(%d) = %v, want %v", tc.pending, got, tc.want)
		}
	}
}
