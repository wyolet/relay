package coord

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// counter is a participant shaped like a window cell: a local delta synced
// every period beats, base set from the store's total.
type counter struct {
	key    string
	period uint64
	base   atomic.Int64
	delta  atomic.Int64
}

func (c *counter) Collect(t Tick, b *Batch) {
	if t.N%c.period != 0 {
		return
	}
	d := c.delta.Load()
	b.Add(Cmd{Op: OpIncrBy, Key: c.key, Args: [2]int64{d, 0}, Reply: func(r Reply) {
		if r.Err == nil {
			c.base.Store(r.Int)
			c.delta.Add(-d)
		}
	}})
}

// step runs one numbered beat, as Run does.
func (t *Ticker) step(ctx context.Context, due time.Time) error {
	t.n++
	return t.beat(ctx, due, t.n)
}

func newTestTicker(s Store, clk *clock, o TickerOptions) *Ticker {
	t := NewTicker(s, o)
	t.now = clk.now
	t.started.Store(clk.now().UnixNano())
	return t
}

func TestTickerBatchesParticipantsPerPeriod(t *testing.T) {
	s := newFakeStore()
	clk := &clock{t: time.Unix(1000, 0)}
	tk := newTestTicker(s, clk, TickerOptions{})
	fast := &counter{key: "{a}:fast", period: 1}
	slow := &counter{key: "{b}:slow", period: 4}
	tk.Register(fast)
	tk.Register(slow)

	fast.delta.Add(3)
	slow.delta.Add(5)
	for range 4 {
		if err := tk.step(context.Background(), clk.add(50*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if s.pipelines != 4 || len(s.sent[0]) != 1 || len(s.sent[3]) != 2 {
		t.Fatalf("pipelines = %d, first = %d cmds, fourth = %d cmds", s.pipelines, len(s.sent[0]), len(s.sent[3]))
	}
	if fast.base.Load() != 3 || fast.delta.Load() != 0 || slow.base.Load() != 5 || slow.delta.Load() != 0 {
		t.Fatalf("fast %d/%d slow %d/%d", fast.base.Load(), fast.delta.Load(), slow.base.Load(), slow.delta.Load())
	}
}

func TestTickerPresenceCountsLivePods(t *testing.T) {
	s := newFakeStore()
	clk := &clock{t: time.Unix(1000, 0)}
	o := TickerOptions{PresenceKey: "{engine}:pods", PresenceTTL: time.Second}
	o.PodID = "a"
	a := newTestTicker(s, clk, o)
	o.PodID = "b"
	b := newTestTicker(s, clk, o)
	if a.Status().LivePods != 1 {
		t.Fatal("LivePods before the first beat is not 1")
	}
	ctx := context.Background()
	_ = a.step(ctx, clk.now())
	_ = b.step(ctx, clk.now())
	_ = a.step(ctx, clk.now())
	if got := a.Status().LivePods; got != 2 {
		t.Fatalf("LivePods = %d, want 2", got)
	}
	// b stops beating; once its entry is older than the TTL, a counts itself.
	clk.add(1500 * time.Millisecond)
	_ = a.step(ctx, clk.now())
	if got := a.Status().LivePods; got != 1 {
		t.Fatalf("LivePods after b left = %d, want 1", got)
	}
	if s.ttls["{engine}:pods"] != 1000 {
		t.Fatalf("presence key ttl = %d", s.ttls["{engine}:pods"])
	}
}

func TestTickerOutageAndRecovery(t *testing.T) {
	s := newFakeStore()
	clk := &clock{t: time.Unix(1000, 0)}
	var outages, recoveries atomic.Int32
	var downFor time.Duration
	tk := newTestTicker(s, clk, TickerOptions{Hooks: TickerHooks{
		OnOutage:  func(time.Time) { outages.Add(1) },
		OnRecover: func(d time.Duration) { recoveries.Add(1); downFor = d },
	}})
	c := &counter{key: "k", period: 1}
	tk.Register(c)
	ctx := context.Background()

	_ = tk.step(ctx, clk.now())
	s.setDown(true)
	c.delta.Add(7)
	for range 10 {
		if err := tk.step(ctx, clk.add(50*time.Millisecond)); err == nil {
			t.Fatal("beat against a down store succeeded")
		}
	}
	if st := tk.Status(); st.Outage || outages.Load() != 0 {
		t.Fatal("outage declared inside the grace")
	}
	for range 15 {
		_ = tk.step(ctx, clk.add(50*time.Millisecond))
	}
	if st := tk.Status(); !st.Outage || outages.Load() != 1 {
		t.Fatalf("outage = %v, OnOutage calls = %d", st.Outage, outages.Load())
	}
	if c.delta.Load() != 7 {
		t.Fatal("a failed beat consumed the delta")
	}

	s.setDown(false)
	_ = tk.step(ctx, clk.add(50*time.Millisecond))
	if st := tk.Status(); st.Outage || recoveries.Load() != 1 || downFor != 1300*time.Millisecond {
		t.Fatalf("outage = %v, recoveries = %d, down = %v", st.Outage, recoveries.Load(), downFor)
	}
	if s.counter("k") != 7 || c.delta.Load() != 0 {
		t.Fatal("delta not delivered after recovery")
	}
}

func TestTickerLagCountsAsFailedBeat(t *testing.T) {
	s := newFakeStore()
	clk := &clock{t: time.Unix(1000, 0)}
	var lags []time.Duration
	tk := newTestTicker(s, clk, TickerOptions{Hooks: TickerHooks{OnLag: func(l time.Duration) { lags = append(lags, l) }}})
	due := clk.now()
	clk.add(time.Second)
	if err := tk.step(context.Background(), due); err == nil {
		t.Fatal("a lagged beat counted as good")
	}
	if len(lags) != 1 || lags[0] != time.Second || tk.Status().LastDelay != time.Second {
		t.Fatalf("lags = %v, LastDelay = %v", lags, tk.Status().LastDelay)
	}
	if !tk.Status().LastGood.IsZero() {
		t.Fatal("LastGood set by a lagged beat")
	}
}

func TestTickerCloseFlushes(t *testing.T) {
	s := newFakeStore()
	tk := NewTicker(s, TickerOptions{MinPeriod: time.Millisecond})
	c := &counter{key: "k", period: 1_000_000}
	tk.Register(c)
	done := make(chan error, 1)
	go func() { done <- tk.Run(context.Background()) }()
	c.delta.Add(9)
	time.Sleep(5 * time.Millisecond)
	if err := tk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if s.counter("k") != 9 {
		t.Fatalf("store total after Close = %d", s.counter("k"))
	}
	if err := tk.Run(context.Background()); err == nil {
		t.Fatal("Run after Close succeeded")
	}
}
