// Package hybrid implements limits.Limiter by counting in pod memory, synced with the shared store once per interval, and by counting through the Lua limiter (app/limits/lua) the counters a stale count could push past their cap.
//
// A counter is counted locally when it keeps a plain store counter (a fixed or sliding window, a token meter, a budget), its cap is at least 100 and, for a window rule, its window is longer than the sync interval. A local counter admits against the fleet total its last sync read plus this pod's unsynced count, with no store call. Each interval one pipelined round trip adds every unsynced count to the counter's own store key and reads the totals back. From those totals the sync puts a counter in exact mode once it is at 80% of its cap, or once less room is left than one interval can spend (the fleet's spend over the last interval or, for a window rule, its amount per interval, whichever is larger); a later sync or a new window that finds it clear of both returns it to local mode. Exact counters, and counters whose totals were never read or are older than two intervals, are reserved per request by the Lua scripts; their unsynced count rides the same pipeline ahead of the scripts, so the scripts see it and nothing counts twice. Token buckets, leaky buckets, session windows and concurrency leases are always exact.
//
// Local counts reuse the Lua limiter's store keys and TTLs, so a fleet switched between the two keeps its counts. Refunds follow the Lua limiter: a refused or released request leaves nothing counted.
//
// Scope: the counter table, the sync loop, the mode switch and settling local counts. Out of scope: the scripts, key formats and window arithmetic (pkg/ratelimit), and which counters a request touches (app/plan).
//
// Expected kv ops per request:
//   - Admit: none when every counter is local. Otherwise one pipelined round trip: an INCRBY and EXPIRE NX per bucket whose unsynced count is handed over, then one script per group of exact counters. A refusal after a group reserved adds one batched round trip releasing it.
//   - Settle and Release: none for local counters; the Lua limiter's batched commit for exact ones.
//   - Peek (admin reads, off the request path): none for counters with a synced local slot; the Lua limiter's one batched read for the rest.
//
// Per pod and interval, whatever the request rate: one pipelined round trip covering the counters touched since the last sync, an INCRBY and EXPIRE NX per bucket with an unsynced count and a GET per other bucket read.
package hybrid

import (
	"context"
	"sync"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

const (
	// minLocalAmount is the smallest cap counted locally: below it one interval's overshoot is a large share of the cap.
	minLocalAmount = 100
	// idleAfter is how long a counter nobody touches stays in the table once its counts are synced.
	idleAfter = 5 * time.Minute
	// syncTimeout bounds one sync round trip, and the final one at Close.
	syncTimeout = 2 * time.Second
)

// DefaultInterval is the sync interval when New is given none.
const DefaultInterval = time.Second

// Limiter counts locally where it can and through the Lua limiter elsewhere. One Limiter is shared across the process; concurrent calls are safe.
type Limiter struct {
	exact *lua.Limiter
	// store runs the exact scripts and the handoffs queued ahead of them: the one the Lua limiter admits on.
	store kv.Store
	// syncStore runs the sync, off the admission pool.
	syncStore kv.Store
	interval  time.Duration
	clock     func() time.Time

	mu       sync.Mutex
	tab      table
	seq      uint32
	lastSync int64 // unix ns the last sync was sent

	syncing  sync.Mutex
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

var _ limits.Limiter = (*Limiter)(nil)

// New returns a Limiter whose exact counters go through exact, which admits on store; syncs run on syncStore, which may be store. interval 0 means DefaultInterval. Call Start to run the sync loop.
func New(exact *lua.Limiter, store, syncStore kv.Store, interval time.Duration) *Limiter {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Limiter{
		exact: exact, store: store, syncStore: syncStore, interval: interval, clock: time.Now,
		tab: newTable(), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// WithClock replaces time.Now, for tests; it must agree with the clock of the Lua limiter's pkg/ratelimit.Limiter. Call before serving.
func (l *Limiter) WithClock(clock func() time.Time) *Limiter {
	l.clock = clock
	return l
}

// Start runs the sync loop until Close. Call once.
func (l *Limiter) Start() {
	l.started = true
	go l.run()
}

func (l *Limiter) run() {
	defer close(l.done)
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.sync()
		}
	}
}

// Close stops the sync loop and adds every unsynced count to the store, so a pod that stops cleanly loses none.
func (l *Limiter) Close() {
	l.stopOnce.Do(func() { close(l.stop) })
	if l.started {
		<-l.done
	}
	l.sync()
}

// Admit checks the local counters in memory and reserves the others through the Lua scripts, in one round trip with the unsynced counts they take over. A local refusal makes no store call; an exact refusal or store failure undoes the local counts.
func (l *Limiter) Admit(ctx context.Context, cs []limits.Counter, _ limits.Usage) (limits.Reservation, error) {
	if len(cs) == 0 {
		return noCounters{}, nil
	}
	now := l.clock()
	res := &reservation{lim: l, counters: cs}
	res.local = res.localBuf[:0]
	var (
		exactBuf   [8]int
		staleBuf   [4]hold
		handoffBuf [4]push
	)
	exact, stale, handoffs := exactBuf[:0], staleBuf[:0], handoffBuf[:0]

	l.mu.Lock()
	for i := range cs {
		c := &cs[i]
		if counted(cs[:i], c) {
			continue
		}
		r, err := lua.Rule(c)
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		idx, ok := l.localSlot(c, r, now)
		if !ok {
			exact = append(exact, i)
			continue
		}
		s := &l.tab.slots[idx]
		if !l.usable(s, now) {
			exact = append(exact, i)
			stale = append(stale, hold{counter: i, slot: idx})
			continue
		}
		if retry, fits := pkgratelimit.CheckPlain(r, s.cur.total(), s.prev.total(), now); !fits {
			l.mu.Unlock()
			return nil, &limits.Refused{Counter: c, RetryAfter: retry}
		}
		res.local = append(res.local, hold{counter: i, slot: idx, gen: s.gen, bucket: s.cur.start})
	}
	for _, h := range res.local {
		if s := &l.tab.slots[h.slot]; countsRequests(s.kind) {
			s.cur.delta++
		}
	}
	// An exact counter's unsynced count goes to the store ahead of its script, so the script sees it.
	for _, h := range stale {
		handoffs = l.takePending(handoffs, h.slot, h.counter)
	}
	l.mu.Unlock()

	if len(exact) == 0 {
		return res, nil
	}
	sub := cs
	if len(res.local) > 0 {
		sub = make([]limits.Counter, len(exact))
		for j, i := range exact {
			sub[j] = cs[i]
		}
	}
	b := kv.NewBatch(l.store, 2*len(handoffs)+2)
	queuePushes(b, handoffs, func(p *push) (string, pkgratelimit.Rule, bool) {
		c := &cs[p.ref]
		r, err := lua.Rule(c)
		return c.Group, r, err == nil
	}, now)
	prepared, err := l.exact.PrepareAdmit(b, sub)
	if err != nil {
		l.ackPushes(handoffs, false)
		res.undoLocal()
		return nil, err
	}
	b.Exec(ctx)
	l.ackPushes(handoffs, true)
	held, err := prepared.Finish(ctx)
	if err != nil {
		res.undoLocal()
		return nil, err
	}
	res.exact = held
	return res, nil
}

// counted reports whether an earlier counter of the request has c's identity, which counts once.
func counted(earlier []limits.Counter, c *limits.Counter) bool {
	for i := range earlier {
		if earlier[i].Group == c.Group && earlier[i].ID == c.ID {
			return true
		}
	}
	return false
}

func countsRequests(k pkgratelimit.PlainCounter) bool {
	return k == pkgratelimit.PlainFixedWindow || k == pkgratelimit.PlainSlidingWindow
}

// localSlot returns the slot that counts c here, moved to the bucket containing now, and false for a counter that is always exact.
func (l *Limiter) localSlot(c *limits.Counter, r pkgratelimit.Rule, now time.Time) (uint32, bool) {
	kind := pkgratelimit.PlainCounterOf(r)
	if kind == pkgratelimit.NotPlain || r.Amount < minLocalAmount || (kind != pkgratelimit.PlainSpend && r.Window <= l.interval) {
		return 0, false
	}
	h := l.tab.hash(c.Group, c.ID, r.Meter)
	idx, found, collided := l.tab.find(h, c.Group, c.ID, r.Meter)
	if collided {
		return 0, false
	}
	if !found {
		start, err := pkgratelimit.BucketStart(r, now)
		if err != nil {
			return 0, false
		}
		var ok bool
		if idx, ok = l.tab.add(h, c.Group, c.ID, r.Meter); !ok {
			return 0, false
		}
		s := &l.tab.slots[idx]
		s.kind, s.window, s.period = kind, r.Window, periodCode(r.Period)
		if kind == pkgratelimit.PlainSpend {
			s.window = 0
		}
		s.cur.start = start.UnixMilli()
		s.prev.start = previousStart(s.shape(), s.cur.start)
	}
	s := &l.tab.slots[idx]
	// An amount or budget-mode edit keeps the counter's identity, so these follow the latest request.
	s.amount, s.alert, s.rung = r.Amount, r.Alert, rungCode(c.Rung)
	s.touchedAt = now.UnixNano()
	l.roll(s, now)
	return idx, true
}

// usable reports whether s may admit from memory at now: in local mode, with totals read within the last two intervals.
func (l *Limiter) usable(s *slot, now time.Time) bool {
	return !s.exact && s.syncedAt != 0 && now.UnixNano()-s.syncedAt <= int64(2*l.interval)
}

// roll moves s to the bucket containing now. A move by one bucket keeps the current bucket as the previous one, unsynced count included; a longer gap leaves s unread, so it counts exactly until the next sync.
func (l *Limiter) roll(s *slot, now time.Time) {
	shape := s.shape()
	t, err := pkgratelimit.BucketStart(shape, now)
	if err != nil {
		return
	}
	start := t.UnixMilli()
	if start == s.cur.start {
		return
	}
	prevStart := previousStart(shape, start)
	if prevStart == s.cur.start {
		s.prev, s.cur = s.cur, bucket{start: start}
	} else {
		s.prev, s.cur = bucket{start: prevStart}, bucket{start: start}
		s.syncedAt = 0
	}
	s.exact = l.nearCap(s, now)
}

func previousStart(shape pkgratelimit.Rule, start int64) int64 {
	t, _ := pkgratelimit.BucketStart(shape, time.UnixMilli(start-1))
	return t.UnixMilli()
}

// nearCap reports whether s should count exactly: at 80% of its cap, or with less room than one interval can spend. Alert budgets never refuse, so they always count locally.
func (l *Limiter) nearCap(s *slot, now time.Time) bool {
	if s.alert {
		return false
	}
	count := pkgratelimit.PlainCount(s.shape(), s.cur.total(), s.prev.total(), now)
	amount := float64(s.amount)
	if count >= 0.8*amount {
		return true
	}
	perSync := float64(s.perSync)
	if s.window > 0 {
		perSync = max(perSync, amount*float64(l.interval)/float64(s.window))
	}
	return amount-count < perSync
}

// rungs are the rungs the budget-crossing metric is labelled by; a slot stores the index.
var rungs = [...]limits.Rung{"", limits.RungAccount, limits.RungTeam, limits.RungProject, limits.RungLane, limits.RungSystem}

func rungCode(r limits.Rung) uint8 {
	for i, have := range rungs {
		if have == r {
			return uint8(i)
		}
	}
	return 0
}

func budgetMode(alert bool) budget.Mode {
	if alert {
		return budget.ModeAlert
	}
	return budget.ModeHard
}

// noCounters is the reservation of a request with nothing to count.
type noCounters struct{}

func (noCounters) Settle(limits.Usage) {}
func (noCounters) Release()            {}
