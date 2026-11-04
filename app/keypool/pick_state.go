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

// DefaultRefreshInterval is how often a PickState rereads the breaker records of the keys picked since its last refresh. It bounds how late another pod's breaker transition reaches this one.
const DefaultRefreshInterval = time.Second

// pickStateIdle is how long a record nobody picks stays in a PickState once read.
const pickStateIdle = 5 * time.Minute

// PickState is the pick state a pod keeps in memory instead of reading the store per request: a copy of the breaker records of the keys it picks, reread off the request path once per interval, and the round-robin rotation and least-recently-used stamps of each pick scope, which are this pod's own. Selectors sharing one PickState (the request path's and the post-flight one) see each other's transitions at once.
//
// A record not yet read, or last read more than two intervals ago (refreshes failing), is read through by the pick that needs it.
type PickState struct {
	// store is where refreshes read, off the admission pool.
	store    kv.Store
	interval time.Duration
	clock    func() time.Time
	log      *slog.Logger

	records sync.Map // key hash → *cachedRecord
	// rotation is each scope's round-robin counter; lastUse each scope and key's last pick, unix ms.
	rotation sync.Map // scope → *atomic.Int64
	lastUse  sync.Map // lastUseKey → *atomic.Int64

	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

type cachedRecord struct {
	snap atomic.Pointer[recordSnap]
	// picked is set by every pick that reads the record and cleared by the refresh that rereads it.
	picked atomic.Bool
}

// recordSnap is a record as the store held it at a moment: a read, or a transition this pod wrote.
type recordSnap struct {
	rec   CircuitRecord
	found bool
	at    int64 // unix ns
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

// Refresh rereads, in one pipelined round trip, the records of the keys picked since the last refresh, and forgets the ones nobody picked for pickStateIdle. A read that fails leaves its record as it was, to age into a read-through.
func (ps *PickState) Refresh(ctx context.Context) {
	now := ps.clock()
	var keys []string
	ps.records.Range(func(k, v any) bool {
		c := v.(*cachedRecord)
		if c.picked.Swap(false) {
			keys = append(keys, k.(string))
		} else if s := c.snap.Load(); s != nil && now.UnixNano()-s.at > int64(pickStateIdle) {
			ps.records.Delete(k)
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
		ps.remember(k, rec, found, now, false)
	}
	if failed > 0 {
		ps.log.Debug("keypool: breaker refresh reads failed", "failed", failed, "keys", len(keys))
	}
}

// fresh returns keyHash's record when it was read or written within two intervals of now, marking it picked so the next refresh rereads it.
func (ps *PickState) fresh(keyHash string, now time.Time) (CircuitRecord, bool, bool) {
	v, ok := ps.records.Load(keyHash)
	if !ok {
		return CircuitRecord{}, false, false
	}
	c := v.(*cachedRecord)
	c.picked.Store(true)
	s := c.snap.Load()
	if s == nil || now.UnixNano()-s.at > int64(2*ps.interval) {
		return CircuitRecord{}, false, false
	}
	return s.rec, s.found, true
}

// remember stores rec as keyHash's record as of at, unless the state already holds a newer one: a refresh read that left before this pod's own transition must not undo it. picked marks the record for the next refresh.
func (ps *PickState) remember(keyHash string, rec CircuitRecord, found bool, at time.Time, picked bool) {
	v, ok := ps.records.Load(keyHash)
	if !ok {
		v, _ = ps.records.LoadOrStore(keyHash, &cachedRecord{})
	}
	c := v.(*cachedRecord)
	if picked {
		c.picked.Store(true)
	}
	next := &recordSnap{rec: rec, found: found, at: at.UnixNano()}
	for {
		cur := c.snap.Load()
		if cur != nil && cur.at > next.at {
			return
		}
		if c.snap.CompareAndSwap(cur, next) {
			return
		}
	}
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
func (ps *PickState) nextRotation(scope string) int64 {
	v, ok := ps.rotation.Load(scope)
	if !ok {
		v, _ = ps.rotation.LoadOrStore(scope, new(atomic.Int64))
	}
	return v.(*atomic.Int64).Add(1)
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
