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
//   - PrepareAdmit: the same commands queued on the caller's kv.Batch, no round trip of its own.
//   - Settle and Release: none for local counters; the Lua limiter's batched commit for exact ones. PrepareSettle queues those commits on the caller's kv.Batch instead.
//   - Peek (admin reads, off the request path): none for counters with a synced local slot; the Lua limiter's one batched read for the rest.
//
// Per pod and interval, whatever the request rate: one pipelined round trip covering the counters touched since the last sync, an INCRBY and EXPIRE NX per bucket with an unsynced count and a GET per other bucket read.
package hybrid

import (
	"context"
	"slices"
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

	// mu guards the table's structure. Requests hold it and their slots' locks shared and count with atomic adds, so requests on the same counter do not wait on one another; adding and dropping slots take it exclusively.
	mu  sync.RWMutex
	tab table

	syncing  sync.Mutex
	lastSync int64 // unix ns the last sync was sent; under syncing
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

var (
	_ limits.Limiter       = (*Limiter)(nil)
	_ limits.BatchAdmitter = (*Limiter)(nil)
)

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
	var (
		exactBuf   [8]int
		staleBuf   [4]hold
		handoffBuf [4]push
	)
	now := l.clock()
	res := &reservation{lim: l, counters: cs}
	exact, handoffs, err := l.checkLocal(res, now, exactBuf[:0], staleBuf[:0], handoffBuf[:0])
	if err != nil || len(exact) == 0 {
		return okReservation(res, err)
	}
	b := kv.NewBatch(l.store, 2*len(handoffs)+2)
	prepared, err := l.queueExact(b, res, exact, handoffs, now)
	if err != nil {
		return nil, err
	}
	b.Exec(ctx)
	return l.finishExact(ctx, res, handoffs, prepared)
}

func okReservation(res *reservation, err error) (limits.Reservation, error) {
	if err != nil {
		return nil, err
	}
	return res, nil
}

// PrepareAdmit is Admit queued on b, a batch on the store the Lua limiter admits on: the local counters are checked now, and the handed-over counts and exact scripts ride b. A local refusal returns at once with nothing queued.
func (l *Limiter) PrepareAdmit(b *kv.Batch, cs []limits.Counter) (limits.PreparedAdmit, error) {
	if len(cs) == 0 {
		return &admission{done: noCounters{}}, nil
	}
	now := l.clock()
	a := &admission{lim: l, res: &reservation{lim: l, counters: cs}}
	exact, handoffs, err := l.checkLocal(a.res, now, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(exact) == 0 {
		a.done = a.res
		return a, nil
	}
	a.handoffs = handoffs
	if a.prepared, err = l.queueExact(b, a.res, exact, handoffs, now); err != nil {
		return nil, err
	}
	return a, nil
}

// admission is a PrepareAdmit waiting for its batch.
type admission struct {
	lim      *Limiter
	res      *reservation
	handoffs []push
	prepared limits.PreparedAdmit
	// done is the reservation when nothing went on the batch.
	done limits.Reservation
}

// Finish returns the reservation once a's batch has run.
func (a *admission) Finish(ctx context.Context) (limits.Reservation, error) {
	if a.done != nil {
		return a.done, nil
	}
	return a.lim.finishExact(ctx, a.res, a.handoffs, a.prepared)
}

// checkLocal admits res's local counters against their synced totals, refusing with no store call, and appends the rest to exact with the unsynced counts they take over to handoffs.
func (l *Limiter) checkLocal(res *reservation, now time.Time, exact []int, stale []hold, handoffs []push) ([]int, []push, error) {
	cs := res.counters
	res.local = res.localBuf[:0]
	var lkBuf [8]lookup
	lks := lkBuf[:0]
	for i := range cs {
		c := &cs[i]
		if counted(cs[:i], c) {
			continue
		}
		r, err := lua.Rule(c)
		if err != nil {
			return nil, nil, err
		}
		lk, ok := l.lookupOf(c, r, now)
		if !ok {
			exact = append(exact, i)
			continue
		}
		lk.counter = i
		lks = append(lks, lk)
	}
	if len(lks) == 0 {
		return exact, handoffs, nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	lks, exact = l.findSlots(cs, lks, exact)
	for j := range lks {
		lk := &lks[j]
		s := &l.tab.slots[lk.slot]
		start, usable, err := l.admitIn(s, lk, &cs[lk.counter], now)
		if err != nil {
			l.undoHolds(res, now)
			return nil, nil, err
		}
		if !usable {
			exact = append(exact, lk.counter)
			stale = append(stale, hold{counter: lk.counter, slot: lk.slot})
			continue
		}
		res.local = append(res.local, hold{counter: lk.counter, slot: lk.slot, gen: s.gen, bucket: start})
	}
	// An exact counter's unsynced count goes to the store ahead of its script, so the script sees it.
	for _, h := range stale {
		s := &l.tab.slots[h.slot]
		s.mu.Lock()
		handoffs = l.takePending(handoffs, h.slot, h.counter)
		s.mu.Unlock()
	}
	// exact was gathered in three passes; the Lua limiter names the first refusing counter in the request's order.
	slices.Sort(exact)
	return exact, handoffs, nil
}

// admitIn checks lk's request against s, its slot, and counts it there when it fits, returning the bucket counted in. usable is false when s must count exactly. A request count is added before the check, so concurrent requests each see the others' counts; a refusal takes it back. Called with mu read-locked.
func (l *Limiter) admitIn(s *slot, lk *lookup, c *limits.Counter, now time.Time) (start int64, usable bool, err error) {
	s.mu.RLock()
	at, ok := l.current(s, lk, now)
	if ok {
		defer s.mu.RUnlock()
	} else {
		s.mu.RUnlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		at = l.touch(s, lk, now)
	}
	if !l.usable(s, now) {
		return 0, false, nil
	}
	reserve := countsRequests(s.kind)
	cur := s.cur.total()
	if reserve {
		cur = s.cur.global + s.cur.delta.Add(1) - 1
	}
	if retry, fits := pkgratelimit.CheckPlain(lk.rule, cur, s.prev.total(), at); !fits {
		if reserve {
			s.cur.delta.Add(-1)
		}
		return 0, true, &limits.Refused{Counter: c, RetryAfter: retry}
	}
	return s.cur.start, true, nil
}

// queueExact queues the handoffs and then the exact counters' scripts on b. On an error the local counts are undone and b must not run.
func (l *Limiter) queueExact(b *kv.Batch, res *reservation, exact []int, handoffs []push, now time.Time) (limits.PreparedAdmit, error) {
	cs := res.counters
	sub := cs
	if len(res.local) > 0 {
		sub = make([]limits.Counter, len(exact))
		for j, i := range exact {
			sub[j] = cs[i]
		}
	}
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
	return prepared, nil
}

// finishExact completes res once the batch carrying its handoffs and exact scripts has run; an exact refusal or store failure undoes the local counts.
func (l *Limiter) finishExact(ctx context.Context, res *reservation, handoffs []push, prepared limits.PreparedAdmit) (limits.Reservation, error) {
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

// lookup is one counter of a request with what finding and moving its slot needs, worked out before any lock is taken.
type lookup struct {
	counter int // index into the request's counters
	rule    pkgratelimit.Rule
	kind    pkgratelimit.PlainCounter
	rung    uint8
	start   int64 // the bucket containing now, unix ms
	at      int64 // now, unix ns
	hash    uint64
	slot    uint32
	found   bool
}

// lookupOf returns c's lookup at now, and false for a counter that is always exact.
func (l *Limiter) lookupOf(c *limits.Counter, r pkgratelimit.Rule, now time.Time) (lookup, bool) {
	kind := pkgratelimit.PlainCounterOf(r)
	if kind == pkgratelimit.NotPlain || r.Amount < minLocalAmount || (kind != pkgratelimit.PlainSpend && r.Window <= l.interval) || !identFits(c.Group, c.ID, r.Meter) {
		return lookup{}, false
	}
	start, err := pkgratelimit.BucketStart(r, now)
	if err != nil {
		return lookup{}, false
	}
	return lookup{rule: r, kind: kind, rung: rungCode(c.Rung), start: start.UnixMilli(), at: now.UnixNano()}, true
}

// findSlots sets the slot of each lookup, adding the slots the table lacks, and returns the lookups that have one; the counters of the others, which cannot be counted here, go to exact. Called with mu read-locked, which it drops and retakes to add.
func (l *Limiter) findSlots(cs []limits.Counter, lks []lookup, exact []int) ([]lookup, []int) {
	for i := range lks {
		c := &cs[lks[i].counter]
		lks[i].hash = l.tab.hash(c.Group, c.ID, lks[i].rule.Meter)
	}
	for attempt := 0; ; attempt++ {
		n, missing := 0, false
		for i := range lks {
			lk := &lks[i]
			c := &cs[lk.counter]
			idx, found, collided := l.tab.find(lk.hash, c.Group, c.ID, lk.rule.Meter)
			// A slot added for it and freed again before this pass is left to the exact path rather than retried.
			if collided || (!found && attempt > 0) {
				exact = append(exact, lk.counter)
				continue
			}
			lk.slot, lk.found = idx, found
			missing = missing || !found
			if n != i {
				lks[n] = *lk
			}
			n++
		}
		lks = lks[:n]
		if !missing {
			return lks, exact
		}
		l.mu.RUnlock()
		l.mu.Lock()
		for i := range lks {
			if !lks[i].found {
				l.addSlot(&cs[lks[i].counter], &lks[i])
			}
		}
		l.mu.Unlock()
		l.mu.RLock()
	}
}

// addSlot adds the slot of c unless another request added it first. Called with mu locked.
func (l *Limiter) addSlot(c *limits.Counter, lk *lookup) {
	if _, found, collided := l.tab.find(lk.hash, c.Group, c.ID, lk.rule.Meter); found || collided {
		return
	}
	idx, ok := l.tab.add(lk.hash, c.Group, c.ID, lk.rule.Meter)
	if !ok {
		return
	}
	s := &l.tab.slots[idx]
	s.kind, s.window, s.period = lk.kind, lk.rule.Window, periodCode(lk.rule.Period)
	if lk.kind == pkgratelimit.PlainSpend {
		s.window = 0
	}
	s.cur.start = lk.start
	s.prev.start = previousStart(s.shape(), s.cur.start)
	// Touched now, or the sync could free it as idle before its caller relocks and counts in it.
	s.touchedAt.Store(lk.at)
}

// current records a request for lk's counter at now on s, its slot, when s needs no change for it: same rule fields, and now in its current bucket or, for a clock read just before another caller moved s on, its previous one. It returns the time s counts the request at, as roll does. Called with s read-locked.
func (l *Limiter) current(s *slot, lk *lookup, now time.Time) (time.Time, bool) {
	if s.amount != lk.rule.Amount || s.alert != lk.rule.Alert || s.rung != lk.rung {
		return now, false
	}
	switch lk.start {
	case s.cur.start:
	case s.prev.start:
		now = time.UnixMilli(s.cur.start)
	default:
		return now, false
	}
	s.touchedAt.Store(lk.at)
	return now, true
}

// touch records a request for lk's counter at now on s, its slot, and moves s to now's bucket. It returns the time s counts the request at. Called with s write-locked.
func (l *Limiter) touch(s *slot, lk *lookup, now time.Time) time.Time {
	// An amount or budget-mode edit keeps the counter's identity, so these follow the latest request.
	s.amount, s.alert, s.rung = lk.rule.Amount, lk.rule.Alert, lk.rung
	s.touchedAt.Store(lk.at)
	return l.roll(s, lk.start, now)
}

// usable reports whether s may admit from memory at now: in local mode, with totals read within the last two intervals.
func (l *Limiter) usable(s *slot, now time.Time) bool {
	return !s.exact && s.syncedAt != 0 && now.UnixNano()-s.syncedAt <= int64(2*l.interval)
}

// roll moves s to the bucket starting at start, the one containing now, and returns the time s counts now at. A move by one bucket keeps the current bucket as the previous one, unsynced count included; a longer gap leaves s unread, so it counts exactly until the next sync. Called with s write-locked.
func (l *Limiter) roll(s *slot, start int64, now time.Time) time.Time {
	switch {
	case start == s.cur.start:
		return now
	case start == s.prev.start:
		// now was read before another caller moved s on; moving back would drop the newer bucket's counts, so now counts at that bucket's start.
		return time.UnixMilli(s.cur.start)
	}
	prevStart := previousStart(s.shape(), start)
	if prevStart == s.cur.start {
		s.prev.copyFrom(&s.cur)
	} else {
		s.prev.reset(prevStart)
		s.syncedAt = 0
	}
	s.cur.reset(start)
	s.exact = l.nearCap(s, now)
	return now
}

// rollTo moves s to the bucket containing now. Called with s write-locked.
func (l *Limiter) rollTo(s *slot, now time.Time) {
	if t, err := pkgratelimit.BucketStart(s.shape(), now); err == nil {
		l.roll(s, t.UnixMilli(), now)
	}
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

func (noCounters) PrepareSettle(*kv.Batch, limits.Usage) limits.PreparedSettle { return lua.NoWrites{} }
