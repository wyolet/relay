package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/hybrid"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/settle"
	appratelimit "github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/meta"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// hostReached signals post-flight's last step, which runs after the settle.
type hostReached chan struct{}

func (h hostReached) Reachable(context.Context, string)         { h <- struct{}{} }
func (hostReached) Unreachable(context.Context, string, string) {}

// A round-robin request on a healthy key with only caller counters: before the upstream call, one round trip carrying the caller hold and the pick's breaker reads with the rotation counter; the credential has no counters, so its hold costs none. Post-flight writes no success for a key that never failed, and settles in one more round trip only when a rule has something to settle.
func TestRunRoundTrips(t *testing.T) {
	requests := appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 100,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
	}
	tokens := appratelimit.Rule{
		Meter: appratelimit.MeterTokens, Amount: 1 << 20,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategySlidingWindow,
	}
	t.Run("requests_only", func(t *testing.T) { runRoundTrips(t, 1, requests) })
	t.Run("with_tokens", func(t *testing.T) { runRoundTrips(t, 2, requests, tokens) })
}

// The caller hold rides with the first pick's reads, but a refused hold ends the request there: in that one round trip, with no probe claimed on a key whose cooldown ended and the account's counters untouched.
func TestRunCallerRefusalMakesNoPick(t *testing.T) { runCallerRefusalMakesNoPick(t, memScriptStore(t)) }

// memScriptStore is a kv.Mem running the limiter's and key pool's scripts through a wrapper.
func memScriptStore(t *testing.T) kvtest.ScriptStore {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	return mem
}

func runCallerRefusalMakesNoPick(t *testing.T, s kvtest.ScriptStore) {
	store := kvtest.NewRecording(s)
	now := time.Now()
	sel := keypool.New(store, slog.Default(), func() time.Time { return now }, nil)
	lim := lua.New(pkgratelimit.New(store, slog.Default(), nil))
	reached := make(hostReached, 1)
	p := &Pipeline{Limiter: lim, Selector: sel, Logger: slog.Default(), HostHealth: reached}

	key := testKey("key-a")
	sel.RecordFailure(context.Background(), key.KeyHash, keypool.FailureAuth, 0)
	now = now.Add(time.Hour) // the key's cooldown ended: the next pick claims its probe
	account := accountCounters("acct", perMinute("rl-tier", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 100))
	full := callerCounter(perMinute("rl-full", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 1))
	spent, err := lim.Admit(context.Background(), full, limits.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	spent.Settle(limits.Usage{})

	var called atomic.Int32
	adapter := reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
		called.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	}}
	run := func(caller []limits.Counter) error {
		res, err := p.Run(context.Background(), &Request{Adapter: adapter, Hops: []plan.RouteHop{buildHop(keypool.KeySelectionPrioritized, caller, testSlot{key: key, upstream: account})}})
		if err == nil {
			_ = res.Body.Close()
		}
		return err
	}

	before, reserves := store.RoundTrips(), len(store.Named("limit.reserve"))
	var refused *CallerLimitedError
	if err := run(full); !errors.As(err, &refused) {
		t.Fatalf("Run = %v, want the caller's refusal", err)
	}
	if trips := store.RoundTrips() - before; trips != 1 {
		t.Fatalf("a refused caller took %d round trips, want 1 (%v)", trips, store.Batches())
	}
	if n := len(store.Named("limit.reserve")) - reserves; n != 1 || len(store.Named("keypool.claim_probe")) != 0 || called.Load() != 0 {
		t.Fatalf("reserves = %d, probe claims = %d, upstream calls = %d; want the caller's reserve alone", n, len(store.Named("keypool.claim_probe")), called.Load())
	}

	// With room, the same pick claims the probe and the request is served.
	if err := run(callerCounter(perMinute("rl-open", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 100))); err != nil {
		t.Fatalf("Run with room: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
	if len(store.Named("keypool.claim_probe")) != 1 || called.Load() != 1 {
		t.Fatalf("probe claims = %d, upstream calls = %d; want 1 each", len(store.Named("keypool.claim_probe")), called.Load())
	}
}

// With a settle queue, a served request's post-flight writes go out in one round trip: the caller's and the account's token commits and the success write closing the probe it was picked on.
func TestRunSettleSharesOneRoundTrip(t *testing.T) {
	store := kvtest.NewRecording(memScriptStore(t))
	trips := runServedProbe(t, store, func() {})
	if batches := store.Batches(); trips != 1 || batches[len(batches)-1] != 3 {
		t.Fatalf("post-flight round trips = %d, batches %v; want one batch of two commits and the success write", trips, batches)
	}
}

// replyLost runs a batch armed with lose and then fails every command in transport, as a connection dropped after sending would.
type replyLost struct {
	kvtest.ScriptStore
	lose atomic.Bool
}

func (r *replyLost) ExecBatch(ctx context.Context, ops []kv.BatchOp) {
	kv.ExecInOrder(ctx, r.ScriptStore, ops)
	if r.lose.CompareAndSwap(true, false) {
		for _, op := range ops {
			op.Result.Value, op.Result.Err = nil, kv.ErrUnavailable
		}
	}
}

// A settle whose shared round trip lost its replies resends the guarded commits but never the success write, which would take a second backoff step off.
func TestRunSettleNeverResendsSuccess(t *testing.T) {
	lossy := &replyLost{ScriptStore: memScriptStore(t)}
	store := kvtest.NewRecording(lossy)
	runServedProbe(t, store, func() { lossy.lose.Store(true) })
	if n := len(store.Named("keypool.record_success")); n != 1 {
		t.Fatalf("success writes = %d, want 1", n)
	}
	if n := len(store.Named("limit.commit")); n != 4 {
		t.Fatalf("commits = %d, want the two batched and both resent", n)
	}
}

// runServedProbe serves one request on a key whose cooldown ended, so its settle carries the caller's and the account's token commits and the success write closing the probe; arm runs just before the settle. It returns the post-flight's round trips.
func runServedProbe(t *testing.T, store *kvtest.Recording, arm func()) (postFlightTrips int) {
	now := time.Now()
	sel := keypool.New(store, slog.Default(), func() time.Time { return now }, nil)
	q := settle.New(store, settle.Config{BatchSize: 64, QueueSize: 16}, slog.Default())
	q.Start()
	t.Cleanup(q.Close)
	reached := make(hostReached, 1)
	p := &Pipeline{Limiter: lua.New(pkgratelimit.New(store, slog.Default(), nil)), Selector: sel, Logger: slog.Default(), HostHealth: reached, Settles: q}

	key := testKey("key-a")
	sel.RecordFailure(context.Background(), key.KeyHash, keypool.FailureAuth, 0)
	now = now.Add(time.Hour)
	tokens := func(id string) *appratelimit.RateLimit {
		return perMinute(id, appratelimit.MeterTokens, appratelimit.StrategySlidingWindow, 1<<20)
	}
	hop := buildHop(keypool.KeySelectionPrioritized, callerCounter(tokens("rl-caller")), testSlot{key: key, upstream: accountCounters("acct", tokens("rl-tier"))})
	res, err := p.Run(context.Background(), &Request{Adapter: reservationTestAdapter{}, Hops: []plan.RouteHop{hop}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	arm()
	before := store.RoundTrips()
	_ = res.Body.Close()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
	postFlightTrips = store.RoundTrips() - before
	if rec, found := sel.ReadCircuit(context.Background(), key.KeyHash); !found || rec.State != keypool.CircuitClosed {
		t.Fatalf("record after the served probe = %+v (found %v), want closed", rec, found)
	}
	return postFlightTrips
}

// With the hybrid limiter counting every counter in memory and the pick state warm, a round-robin request makes no store call before the upstream: the caller hold, the pick and the account's admit all answer from this process.
func TestRunWarmMakesNoStoreCall(t *testing.T) { runWarmMakesNoStoreCall(t, memScriptStore(t)) }

func runWarmMakesNoStoreCall(t *testing.T, s kvtest.ScriptStore) {
	// Admission and picks run on the recorded store; the sync and refreshes on s, as on the post-flight client.
	store := kvtest.NewRecording(s)
	ps := keypool.NewPickState(s, time.Minute, slog.Default())
	h := hybrid.New(lua.New(pkgratelimit.New(store, slog.Default(), nil)), store, s, time.Second)
	p := &Pipeline{Limiter: h, Selector: keypool.New(store, slog.Default(), nil, nil).WithPickState(ps), Logger: slog.Default()}

	// An hour's window leaves far more room than one sync interval spends, so the counters stay local.
	requests := func(id string) *appratelimit.RateLimit {
		return &appratelimit.RateLimit{Meta: meta.Metadata{ID: id, Name: id}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
			{Meter: appratelimit.MeterRequests, Amount: 1000, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
		}}}
	}
	account := accountCounters("acct", requests("rl-tier"))
	hop := buildHop(keypool.KeySelectionRoundRobin, callerCounter(requests("rl-caller")),
		testSlot{key: testKey("key-a"), upstream: account}, testSlot{key: testKey("key-b"), upstream: account})
	run := func() int {
		var atCall int
		before := store.RoundTrips()
		res, err := p.Run(context.Background(), &Request{
			Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
				atCall = store.RoundTrips() - before
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
			}},
			Hops: []plan.RouteHop{hop},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = res.Body.Close()
		return atCall
	}

	if n := run(); n == 0 {
		t.Fatal("a cold request made no store call: the counters and breaker records were never read")
	}
	// The final sync reads every counter's total, so from here each one counts in memory.
	h.Close()
	for i := range 4 {
		if n := run(); n != 0 {
			t.Fatalf("warm request %d made %d store round trips before the upstream, want 0 (%v)", i, n, store.Batches())
		}
	}
}

func runRoundTrips(t *testing.T, want int, rules ...appratelimit.Rule) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	store := kvtest.NewRecording(mem)

	rl := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-inbound", Name: "inbound"}, Spec: appratelimit.Spec{Rules: rules}}
	hop := buildHop(keypool.KeySelectionRoundRobin, callerCounter(rl), testSlot{key: testKey("key-a")}, testSlot{key: testKey("key-b")})
	reached := make(hostReached, 1)
	p := &Pipeline{
		Limiter:    lua.New(pkgratelimit.New(store, slog.Default(), nil)),
		Selector:   keypool.New(store, slog.Default(), nil, nil),
		Logger:     slog.Default(),
		HostHealth: reached,
	}

	var tripsAtCall int
	res, err := p.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			tripsAtCall = store.RoundTrips()
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
		}},
		Hops: []plan.RouteHop{hop},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tripsAtCall != 1 {
		t.Fatalf("kv round trips before the upstream call = %d, want 1 (%v)", tripsAtCall, store.Batches())
	}
	_ = res.Body.Close()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
	if trips := store.RoundTrips(); trips != want {
		t.Fatalf("kv round trips for the request = %d, want %d (%d scripts: %v)", trips, want, len(store.Scripts()), store.Batches())
	}
	if n := len(store.Named("keypool.record_success")); n != 0 {
		t.Fatalf("success writes = %d, want none for a key that never failed", n)
	}
}
