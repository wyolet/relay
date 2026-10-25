package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/keypool"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

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

// queueFixture: a policy with slots concurrency slots per team, so a released lease is what shows a settle landed.
type queueFixture struct {
	svc   *Service
	store *kvtest.Recording
	pol   *Policy
}

func newQueueFixture(t testing.TB, store kvtest.ScriptStore, slots int64) queueFixture {
	t.Helper()
	rule := concurrencyRule()
	rule.Amount = slots
	return newQueueFixtureRule(t, store, rule)
}

func newQueueFixtureRule(t testing.TB, store kvtest.ScriptStore, rule appratelimit.Rule) queueFixture {
	t.Helper()
	pol := fix("caller")
	pol.Meta.ID = "pol-1"
	rl := testRateLimit("rl-1", rule)
	pol.Spec.RateLimitID = rl.Meta.ID
	rec := kvtest.NewRecording(store)
	svc := NewService(reserveSnap{pol: pol, rl: rl}, keypool.New(rec, discardLogger(), nil, nil), pkgratelimit.New(rec, discardLogger(), nil))
	return queueFixture{svc: svc, store: rec, pol: pol}
}

func (f queueFixture) reserve(t *testing.T, team string, n int) []*pkgratelimit.Reservation {
	t.Helper()
	out := make([]*pkgratelimit.Reservation, n)
	for i := range out {
		res, err := f.svc.ReserveInbound(context.Background(), InboundInput{Policy: f.pol, TeamID: team})
		if err != nil || res == nil {
			t.Fatalf("reserve %d for %s = %v, %v; want a lease", i, team, res, err)
		}
		out[i] = res
	}
	return out
}

func (f queueFixture) assertFull(t *testing.T, team string) {
	t.Helper()
	if _, err := f.svc.ReserveInbound(context.Background(), InboundInput{Policy: f.pol, TeamID: team}); !errors.Is(err, pkgratelimit.ErrExceeded) {
		t.Fatalf("reserve for %s = %v, want ErrExceeded while its leases are held", team, err)
	}
}

// unstartedQueue attaches a queue whose writers start only when the test calls start, so what sits in the channel is under the test's control.
func (f queueFixture) unstartedQueue(t *testing.T, cfg SettleQueueConfig) *settleQueue {
	q := newSettleQueue(f.svc, cfg, discardLogger())
	f.svc.settleQueue = q
	t.Cleanup(func() { q.close(time.Second) })
	return q
}

// settleAll settles each reservation from its own goroutine and returns the outcomes once all are back.
func (f queueFixture) settleAll(res []*pkgratelimit.Reservation) []error {
	errs := make([]error, len(res))
	var wg sync.WaitGroup
	for i, r := range res {
		wg.Go(func() { errs[i] = f.svc.Settle(context.Background(), r, nil, pkgratelimit.Observations{}) })
	}
	wg.Wait()
	return errs
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

// runSettleQueueSuite pins the settle queue on one backend: settles share a round trip, none is lost when the queue is full or closing, and each gets its own outcome.
func runSettleQueueSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	t.Run("full_batch_is_one_round_trip", func(t *testing.T) {
		f := newQueueFixture(t, newStore(t), 4)
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 4, MaxDelay: time.Hour, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		res := f.reserve(t, "team-1", 4)
		f.assertFull(t, "team-1")
		trips := f.store.RoundTrips()
		for i, err := range f.settleAll(res) {
			if err != nil {
				t.Fatalf("settle %d: %v", i, err)
			}
		}
		if got, batches := f.store.RoundTrips()-trips, f.store.Batches(); got != 1 || batches[len(batches)-1] != 4 {
			t.Fatalf("round trips = %d, batches = %v; want one batch of the 4 commits", got, batches)
		}
		f.reserve(t, "team-1", 4)
	})

	t.Run("partial_batch_goes_after_the_delay", func(t *testing.T) {
		f := newQueueFixture(t, newStore(t), 1)
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 64, MaxDelay: 5 * time.Millisecond, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		res := f.reserve(t, "team-1", 1)
		if err := f.svc.Settle(context.Background(), res[0], nil, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("full_queue_settles_directly", func(t *testing.T) {
		f := newQueueFixture(t, newStore(t), 2)
		q := f.unstartedQueue(t, SettleQueueConfig{BatchSize: 8, QueueSize: 1})
		res := f.reserve(t, "team-1", 2)
		queued := make(chan error, 1)
		go func() { queued <- f.svc.Settle(context.Background(), res[0], nil, pkgratelimit.Observations{}) }()
		waitFor(t, "the first settle to fill the queue", func() bool { return len(q.in) == 1 })

		full := testutil.ToFloat64(metrics.SettleQueueFull)
		trips := f.store.RoundTrips()
		if err := f.svc.Settle(context.Background(), res[1], nil, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("Settle with the queue full: %v", err)
		}
		if got := testutil.ToFloat64(metrics.SettleQueueFull) - full; got != 1 {
			t.Fatalf("settle_queue_full_total moved by %v, want 1", got)
		}
		if got := f.store.RoundTrips() - trips; got != 1 {
			t.Fatalf("round trips = %d, want the direct settle's one", got)
		}
		// The direct settle released its lease; the queued one still holds its own.
		f.reserve(t, "team-1", 1)
		f.assertFull(t, "team-1")

		q.start(1)
		if err := <-queued; err != nil {
			t.Fatalf("queued Settle: %v", err)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("close_drains_the_queue", func(t *testing.T) {
		f := newQueueFixture(t, newStore(t), 3)
		q := f.unstartedQueue(t, SettleQueueConfig{BatchSize: 64, MaxDelay: time.Hour, QueueSize: 8})
		res := f.reserve(t, "team-1", 3)
		outcomes := make(chan error, len(res))
		for _, r := range res {
			go func() { outcomes <- f.svc.Settle(context.Background(), r, nil, pkgratelimit.Observations{}) }()
		}
		waitFor(t, "the settles to queue", func() bool { return len(q.in) == len(res) })

		q.start(1)
		// The batch waits on an hour's delay: only the close sends it.
		f.svc.CloseSettleQueue()
		for range res {
			if err := <-outcomes; err != nil {
				t.Fatalf("Settle drained at close: %v", err)
			}
		}
		res = f.reserve(t, "team-1", 3)

		// A closed queue turns settles direct, which is not a full queue.
		full := testutil.ToFloat64(metrics.SettleQueueFull)
		if err := f.svc.Settle(context.Background(), res[0], nil, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("Settle after close: %v", err)
		}
		if got := testutil.ToFloat64(metrics.SettleQueueFull) - full; got != 0 {
			t.Fatalf("settle_queue_full_total moved by %v after close, want 0", got)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("failed_command_fails_only_its_settle", func(t *testing.T) {
		f := newQueueFixture(t, &failingCommands{ScriptStore: newStore(t), marker: "team-bad"}, 1)
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 2, MaxDelay: time.Hour, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		good, bad := f.reserve(t, "team-good", 1), f.reserve(t, "team-bad", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		errs := f.settleAll(append(good, bad...))
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 0 {
			t.Fatalf("settle_retries_total moved by %v for an error reply, want 0", got)
		}
		if errs[0] != nil {
			t.Fatalf("good settle: %v", errs[0])
		}
		if !errors.Is(errs[1], errInjected) {
			t.Fatalf("bad settle = %v, want the injected failure", errs[1])
		}
		if batches := f.store.Batches(); batches[len(batches)-1] != 2 {
			t.Fatalf("batches = %v, want both settles in one", batches)
		}
		f.reserve(t, "team-good", 1)
	})

	t.Run("unsent_batch_is_resent", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t)}
		f := newQueueFixture(t, store, 1)
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 8, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		res := f.reserve(t, "team-1", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		if err := f.svc.Settle(context.Background(), res[0], nil, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		f.reserve(t, "team-1", 1)
	})

	t.Run("resent_commit_that_ran_counts_once", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t), ran: true}
		f := newQueueFixtureRule(t, store, appratelimit.Rule{
			Meter: appratelimit.MeterTokens, Amount: 100,
			Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow,
		})
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 8, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		obs := pkgratelimit.Observations{Tokens: map[string]int64{"input": 40, "output": 20}}
		res := f.reserve(t, "team-1", 1)
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		if err := f.svc.Settle(context.Background(), res[0], nil, obs); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		// 60 of 100 tokens: counted twice, the budget would already be spent.
		res = f.reserve(t, "team-1", 1)
		if err := f.svc.Settle(context.Background(), res[0], nil, obs); err != nil {
			t.Fatalf("second Settle: %v", err)
		}
		f.assertFull(t, "team-1")
	})

	t.Run("resent_success_write_is_harmless", func(t *testing.T) {
		store := &unavailableOnce{ScriptStore: newStore(t), ran: true}
		f := newAdmitFixture(t, store, keypool.KeySelectionPrioritized, 10, concurrencyRule())
		f.svc.WithSettleQueue(SettleQueueConfig{BatchSize: 8, QueueSize: 16}, discardLogger())
		t.Cleanup(f.svc.CloseSettleQueue)
		ctx := context.Background()
		sel := keypool.New(f.store, discardLogger(), nil, nil)
		// Open with a cooldown that has already run out: the next pick probes, and its settle writes the success.
		sel.RecordFailure(ctx, f.keys[0].KeyHash, keypool.FailureRateLimitLong, 0)
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
			t.Fatalf("Admit = %+v, %v; want the probe on the first key", adm, err)
		}
		retries := testutil.ToFloat64(metrics.SettleRetries)
		store.arm()
		if err := f.svc.Settle(ctx, adm.Inbound, adm.Acq, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := testutil.ToFloat64(metrics.SettleRetries) - retries; got != 1 {
			t.Fatalf("settle_retries_total moved by %v, want 1", got)
		}
		if rec, found := sel.ReadCircuit(ctx, f.keys[0].KeyHash); !found || rec.State != keypool.CircuitClosed {
			t.Fatalf("record after a resent success = %+v (found %v), want closed", rec, found)
		}
	})

	t.Run("caller_deadline_does_not_lose_the_settle", func(t *testing.T) {
		f := newQueueFixture(t, newStore(t), 1)
		q := f.unstartedQueue(t, SettleQueueConfig{BatchSize: 8, QueueSize: 4})
		res := f.reserve(t, "team-1", 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.svc.Settle(ctx, res[0], nil, pkgratelimit.Observations{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Settle with a cancelled context = %v, want context.Canceled", err)
		}
		q.start(1)
		waitFor(t, "the abandoned settle to land", func() bool {
			_, err := f.svc.ReserveInbound(context.Background(), InboundInput{Policy: f.pol, TeamID: "team-1"})
			return err == nil
		})
	})
}

func TestSettleQueueMem(t *testing.T) {
	runSettleQueueSuite(t, func(t *testing.T) kvtest.ScriptStore { return admitMem(t) })
}

// queueSettles turns svc's settle queue on when queued, so a suite runs both ways.
func queueSettles(t *testing.T, svc *Service, queued bool) {
	t.Helper()
	if !queued {
		return
	}
	svc.WithSettleQueue(SettleQueueConfig{BatchSize: 64, MaxDelay: time.Millisecond, QueueSize: 64}, discardLogger())
	t.Cleanup(svc.CloseSettleQueue)
}
