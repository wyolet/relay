package coord

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Tick is one scheduler beat. N increases by one per beat; a participant
// syncs an entry when N is a multiple of the entry's period in beats. The
// final beat Close sends has N = 0, which makes every entry due.
type Tick struct {
	N   uint64
	Now time.Time
}

// Batch collects a beat's commands: participants append, the ticker sends
// them all in one pipeline.
type Batch struct{ cmds []Cmd }

// Add appends one command to this beat's pipeline.
func (b *Batch) Add(c Cmd) { b.cmds = append(b.cmds, c) }

// Participant is a cell type's sync adapter. Collect runs on the tick
// goroutine, appends the commands for entries due now with Reply closures
// that apply the store's answer, and must not block.
type Participant interface {
	Collect(t Tick, b *Batch)
}

// TickerOptions configures a Ticker; zero durations take the defaults.
type TickerOptions struct {
	MinPeriod time.Duration // beat period, default 50 ms
	Grace     time.Duration // failed beats tolerated before Outage, default 1 s
	// LagFailAfter is the beat lag past which a beat counts as failed for the
	// grace clock; default 4 × MinPeriod.
	LagFailAfter time.Duration
	// PodID, PresenceKey and PresenceTTL make every beat write this process
	// into a sorted set and count live processes for Status.LivePods; an
	// empty PodID disables presence (LivePods = 1). PresenceTTL defaults to
	// 2 × Grace.
	PodID       string
	PresenceKey string
	PresenceTTL time.Duration
	Hooks       TickerHooks
}

// TickerHooks are the observation points; every field is optional.
type TickerHooks struct {
	OnTick    func(cmds int, dur time.Duration, err error)
	OnLag     func(lag time.Duration) // a beat that failed by lag alone or with an error
	OnOutage  func(since time.Time)   // first failed beat past Grace; since = last good beat
	OnRecover func(down time.Duration)
}

// Status is the failure model participants read at decision time.
type Status struct {
	LastGood  time.Time
	Outage    bool // past Grace with no good beat
	LivePods  int  // from presence at the last good beat; 1 without presence
	LastDelay time.Duration
}

// Ticker drives the beat, sends the pipeline, keeps the failure clock and
// writes presence. One per process.
type Ticker struct {
	s     Store
	o     TickerOptions
	parts []Participant
	batch Batch
	n     uint64
	now   func() time.Time

	podID   []byte
	pods    int64 // presence reply of the current beat
	onPods  func(Reply)
	down    bool
	downRef time.Time

	mu      sync.Mutex
	running bool
	closed  bool
	stop    chan struct{}
	done    chan struct{}

	started   atomic.Int64 // unix nanos; 0 before Run
	lastGood  atomic.Int64
	livePods  atomic.Int64
	lastDelay atomic.Int64
}

// NewTicker returns a ticker over s; Register participants, then Run.
func NewTicker(s Store, o TickerOptions) *Ticker {
	if o.MinPeriod <= 0 {
		o.MinPeriod = 50 * time.Millisecond
	}
	if o.Grace <= 0 {
		o.Grace = time.Second
	}
	if o.LagFailAfter <= 0 {
		o.LagFailAfter = 4 * o.MinPeriod
	}
	if o.PodID != "" {
		if o.PresenceKey == "" {
			panic("coord: PodID without PresenceKey")
		}
		if o.PresenceTTL <= 0 {
			o.PresenceTTL = 2 * o.Grace
		}
	}
	t := &Ticker{s: s, o: o, now: time.Now, podID: []byte(o.PodID), stop: make(chan struct{}), done: make(chan struct{})}
	t.onPods = func(r Reply) {
		if r.Err == nil {
			t.pods = r.Int
		}
	}
	t.livePods.Store(1)
	return t
}

// Register adds a participant; before Run.
func (t *Ticker) Register(p Participant) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running || t.closed {
		panic("coord: Register after Run")
	}
	t.parts = append(t.parts, p)
}

// Run beats every MinPeriod until ctx ends or Close is called; one goroutine.
func (t *Ticker) Run(ctx context.Context) error {
	t.mu.Lock()
	if t.running || t.closed {
		t.mu.Unlock()
		return errors.New("coord: Run called twice or after Close")
	}
	t.running = true
	t.mu.Unlock()
	defer close(t.done)
	t.started.Store(t.now().UnixNano())
	tk := time.NewTicker(t.o.MinPeriod)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.stop:
			return nil
		case due := <-tk.C:
			t.n++
			_ = t.beat(ctx, due, t.n)
		}
	}
}

// Close stops Run, then collects once more and sends with a 2 s deadline, so
// a shutdown loses nothing counted.
func (t *Ticker) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	running := t.running
	t.mu.Unlock()
	if running {
		close(t.stop)
		<-t.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return t.beat(ctx, t.now(), 0)
}

// beat runs one tick: collect, send, judge. due is when the beat was
// scheduled, so a stalled goroutine shows as lag.
func (t *Ticker) beat(ctx context.Context, due time.Time, n uint64) error {
	start := t.now()
	b := &t.batch
	for _, p := range t.parts {
		p.Collect(Tick{N: n, Now: start}, b)
	}
	t.pods = 0
	if t.o.PodID != "" {
		t.addPresence(b, start)
	}
	var err error
	if len(b.cmds) > 0 {
		bctx, cancel := context.WithTimeout(ctx, t.o.Grace)
		err = t.s.Pipeline(bctx, b.cmds)
		cancel()
	}
	sent := len(b.cmds)
	clear(b.cmds)
	b.cmds = b.cmds[:0]

	end := t.now()
	lag := end.Sub(due)
	t.lastDelay.Store(int64(lag))
	if h := t.o.Hooks.OnTick; h != nil {
		h(sent, end.Sub(start), err)
	}
	if err == nil && lag <= t.o.LagFailAfter {
		t.good(end)
		return nil
	}
	if h := t.o.Hooks.OnLag; h != nil && lag > t.o.LagFailAfter {
		h(lag)
	}
	ref := t.lastGoodOrStart()
	if !t.down && !ref.IsZero() && end.Sub(ref) > t.o.Grace {
		t.down = true
		t.downRef = ref
		if h := t.o.Hooks.OnOutage; h != nil {
			h(ref)
		}
	}
	if err == nil {
		err = errors.New("coord: beat lagged")
	}
	return err
}

func (t *Ticker) good(end time.Time) {
	t.lastGood.Store(end.UnixNano())
	if t.pods > 0 {
		t.livePods.Store(t.pods)
	}
	if t.down {
		t.down = false
		if h := t.o.Hooks.OnRecover; h != nil {
			h(end.Sub(t.downRef))
		}
	}
}

func (t *Ticker) addPresence(b *Batch, now time.Time) {
	ms := now.UnixMilli()
	ttl := t.o.PresenceTTL.Milliseconds()
	key := t.o.PresenceKey
	b.Add(Cmd{Op: OpZAdd, Key: key, Args: [2]int64{ms, 0}, Value: t.podID})
	b.Add(Cmd{Op: OpZRemRangeByScore, Key: key, Args: [2]int64{math.MinInt64, ms - ttl}})
	b.Add(Cmd{Op: OpZCount, Key: key, Args: [2]int64{ms - ttl + 1, math.MaxInt64}, Reply: t.onPods})
	b.Add(Cmd{Op: OpPExpire, Key: key, Args: [2]int64{0, ttl}})
}

func (t *Ticker) lastGoodOrStart() time.Time {
	ns := max(t.lastGood.Load(), t.started.Load())
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Status reports the failure model; safe from any goroutine.
func (t *Ticker) Status() Status {
	s := Status{LivePods: int(t.livePods.Load()), LastDelay: time.Duration(t.lastDelay.Load())}
	if ns := t.lastGood.Load(); ns != 0 {
		s.LastGood = time.Unix(0, ns)
	}
	if ref := t.lastGoodOrStart(); !ref.IsZero() {
		s.Outage = t.now().Sub(ref) > t.o.Grace
	}
	return s
}
