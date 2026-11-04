package keypool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// DefaultRefreshInterval is how often a PickState rereads every breaker record it holds. It bounds how late another pod's breaker transition reaches this one.
const DefaultRefreshInterval = time.Second

// pickStateIdle is how long a record, rotation counter or LRU stamp nobody picks stays in a PickState.
const pickStateIdle = 5 * time.Minute

// PickState is the pick state a pod keeps in memory instead of reading the store per request: a copy of the breaker records of the keys it picks, all reread off the request path once per interval, and the round-robin rotation and least-recently-used stamps of each pick scope, which are this pod's own. Selectors sharing one PickState (the request path's and the post-flight one) see each other's transitions at once.
//
// A record not yet read, or last read more than two intervals ago (refreshes failing), is read through by the pick that needs it. A transition the store failed to run is applied here alone and holds against older store reads: an open until its OpenUntil, a success until the store shows a transition after it.
type PickState struct {
	// store is where refreshes read, off the admission pool.
	store    kv.Store
	interval time.Duration
	clock    func() time.Time
	log      *slog.Logger

	records  sync.Map // key hash → *cachedRecord
	rotation sync.Map // scope → *rotationCounter
	lastUse  sync.Map // lastUseKey → *atomic.Int64, the last pick in unix ms

	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

type cachedRecord struct {
	snap atomic.Pointer[recordSnap]
	// pickedAt is when a pick last read the record, unix ns; a record unpicked for pickStateIdle is forgotten.
	pickedAt atomic.Int64
}

// recordSnap is a record as this pod knows it at a moment: a store read, a transition the store ran, or one the store failed to run and this pod applied alone (heldSince set).
type recordSnap struct {
	rec       CircuitRecord
	found     bool
	at        int64 // unix ns
	heldSince int64 // unix ns of a transition applied here alone; 0 otherwise
}

type rotationCounter struct {
	n      atomic.Int64
	usedAt atomic.Int64 // unix ms
}

type lastUseKey struct{ scope, keyHash string }

// NewPickState returns a PickState rereading on store every interval (0 means DefaultRefreshInterval). Call Start to run the refresh loop.
func NewPickState(store kv.Store, interval time.Duration, log *slog.Logger) *PickState {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &PickState{store: store, interval: interval, clock: time.Now, log: log, stop: make(chan struct{}), done: make(chan struct{})}
}

// WithClock replaces time.Now, for tests; it must agree with the clock of the selectors that share the state. Call before serving.
func (ps *PickState) WithClock(clock func() time.Time) *PickState {
	ps.clock = clock
	return ps
}

// Start runs the refresh loop until Close. Call once.
func (ps *PickState) Start() {
	ps.started = true
	go ps.run()
}

func (ps *PickState) run() {
	defer close(ps.done)
	t := time.NewTicker(ps.interval)
	defer t.Stop()
	for {
		select {
		case <-ps.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), ps.interval)
			ps.Refresh(ctx)
			cancel()
		}
	}
}

// Close stops the refresh loop.
func (ps *PickState) Close() {
	ps.stopOnce.Do(func() { close(ps.stop) })
	if ps.started {
		<-ps.done
	}
}

// Refresh rereads every record held, in one pipelined round trip, and forgets the records, rotation counters and LRU stamps nobody picked for pickStateIdle. A read that fails leaves its record as it was, to age into a read-through.
func (ps *PickState) Refresh(ctx context.Context) {
	now := ps.clock()
	idleBefore := now.Add(-pickStateIdle)
	ps.forgetIdle(idleBefore)
	var keys []string
	ps.records.Range(func(k, v any) bool {
		if v.(*cachedRecord).pickedAt.Load() < idleBefore.UnixNano() {
			ps.records.Delete(k)
		} else {
			keys = append(keys, k.(string))
		}
		return true
	})
	if len(keys) == 0 {
		return
	}
	b := kv.NewBatch(ps.store, len(keys))
	reads := make([]*kv.BatchResult, len(keys))
	for i, k := range keys {
		reads[i] = b.Get(circuitKey(k))
	}
	b.Exec(ctx)
	failed := 0
	for i, k := range keys {
		r := reads[i]
		if r.Err != nil && !errors.Is(r.Err, kv.ErrNotFound) {
			failed++
			continue
		}
		rec, found := recordFrom(r.Value, r.Err)
		ps.put(k, recordSnap{rec: rec, found: found, at: now.UnixNano()}, false, false)
	}
	if failed > 0 {
		ps.log.Debug("keypool: breaker refresh reads failed", "failed", failed, "keys", len(keys))
	}
}

func (ps *PickState) forgetIdle(before time.Time) {
	ms := before.UnixMilli()
	ps.rotation.Range(func(k, v any) bool {
		if v.(*rotationCounter).usedAt.Load() < ms {
			ps.rotation.Delete(k)
		}
		return true
	})
	ps.lastUse.Range(func(k, v any) bool {
		if v.(*atomic.Int64).Load() < ms {
			ps.lastUse.Delete(k)
		}
		return true
	})
}

// fresh returns keyHash's record when it was read or written within two intervals of now, marking it picked.
func (ps *PickState) fresh(keyHash string, now time.Time) (CircuitRecord, bool, bool) {
	v, ok := ps.records.Load(keyHash)
	if !ok {
		return CircuitRecord{}, false, false
	}
	c := v.(*cachedRecord)
	c.pickedAt.Store(now.UnixNano())
	s := c.snap.Load()
	if s == nil || now.UnixNano()-s.at > int64(2*ps.interval) {
		return CircuitRecord{}, false, false
	}
	return s.rec, s.found, true
}

// remember keeps rec, read from the store at at, marking it picked when a pick read it.
func (ps *PickState) remember(keyHash string, rec CircuitRecord, found bool, at time.Time, picked bool) {
	ps.put(keyHash, recordSnap{rec: rec, found: found, at: at.UnixNano()}, picked, false)
}

// wrote keeps rec, the record a transition the store ran left there, as of at.
func (ps *PickState) wrote(keyHash string, rec CircuitRecord, at time.Time) {
	ps.put(keyHash, recordSnap{rec: rec, found: true, at: at.UnixNano()}, false, true)
}

// applied keeps rec, a transition the store failed to run, as this pod's own until the store shows a newer one.
func (ps *PickState) applied(keyHash string, rec CircuitRecord, at time.Time) {
	ps.put(keyHash, recordSnap{rec: rec, found: true, at: at.UnixNano(), heldSince: at.UnixNano()}, false, true)
}

// put stores next unless the state holds a newer snapshot: a refresh read that left before this pod's transition must not undo it. A store read (not written) that is no newer than a transition applied here alone keeps that transition, now as of the read.
func (ps *PickState) put(keyHash string, next recordSnap, picked, written bool) {
	v, ok := ps.records.Load(keyHash)
	if !ok {
		c := &cachedRecord{}
		c.pickedAt.Store(next.at)
		v, _ = ps.records.LoadOrStore(keyHash, c)
	}
	c := v.(*cachedRecord)
	if picked {
		c.pickedAt.Store(next.at)
	}
	for {
		cur := c.snap.Load()
		if cur != nil && cur.at > next.at {
			return
		}
		snap := next
		if cur != nil && !written && cur.holds(next) {
			snap = *cur
			snap.at = next.at
		}
		if c.snap.CompareAndSwap(cur, &snap) {
			return
		}
	}
}

// holds reports whether s, a transition applied here alone, still stands against read: an open until its OpenUntil, any transition until the store shows one after it.
func (s *recordSnap) holds(read recordSnap) bool {
	if s.heldSince == 0 {
		return false
	}
	if s.rec.State == CircuitOpen && read.at >= s.rec.OpenUntil.UnixNano() {
		return false
	}
	return !read.found || read.rec.LastTransition.UnixNano() <= s.heldSince
}

// current returns keyHash's record as this pod last saw it, closed when it has none, for a transition the store could not run.
func (ps *PickState) current(keyHash string) CircuitRecord {
	if v, ok := ps.records.Load(keyHash); ok {
		if s := v.(*cachedRecord).snap.Load(); s != nil {
			return s.rec
		}
	}
	return CircuitRecord{State: CircuitClosed}
}

// nextRotation advances scope's round-robin counter and returns its new value, from 1.
func (ps *PickState) nextRotation(scope string, nowMs int64) int64 {
	v, ok := ps.rotation.Load(scope)
	if !ok {
		v, _ = ps.rotation.LoadOrStore(scope, new(rotationCounter))
	}
	r := v.(*rotationCounter)
	r.usedAt.Store(nowMs)
	return r.n.Add(1)
}

// lastUsed returns when this pod last picked keyHash in scope, unix ms; false if never.
func (ps *PickState) lastUsed(scope, keyHash string) (int64, bool) {
	v, ok := ps.lastUse.Load(lastUseKey{scope, keyHash})
	if !ok {
		return 0, false
	}
	return v.(*atomic.Int64).Load(), true
}

func (ps *PickState) stampUse(scope, keyHash string, ms int64) {
	v, ok := ps.lastUse.Load(lastUseKey{scope, keyHash})
	if !ok {
		v, _ = ps.lastUse.LoadOrStore(lastUseKey{scope, keyHash}, new(atomic.Int64))
	}
	v.(*atomic.Int64).Store(ms)
}
