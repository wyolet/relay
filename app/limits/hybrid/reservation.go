package hybrid

import (
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// hold is one local counter of an admitted request.
type hold struct {
	counter   int // index into the request's counters
	slot, gen uint32
	bucket    int64 // start of the bucket a request count went into
}

// reservation holds an admitted request's local counters and, when it had exact ones, the Lua limiter's reservation of them.
type reservation struct {
	lim      *Limiter
	counters []limits.Counter
	local    []hold
	localBuf [6]hold
	exact    limits.Reservation
}

// Settle charges token meters and budgets counted here in memory, at the bucket the settle falls in as the commit script does, and settles the exact counters through the Lua limiter.
func (r *reservation) Settle(actual limits.Usage) {
	if len(r.local) > 0 {
		r.lim.settleLocal(r, actual)
	}
	if r.exact != nil {
		r.exact.Settle(actual)
	}
}

// PrepareSettle charges the local counters now and queues the exact counters' commits on b, a batch on the post-flight store.
func (r *reservation) PrepareSettle(b *kv.Batch, actual limits.Usage) limits.PreparedSettle {
	if len(r.local) > 0 {
		r.lim.settleLocal(r, actual)
	}
	if bs, ok := r.exact.(limits.BatchSettler); ok {
		return bs.PrepareSettle(b, actual)
	}
	if r.exact != nil {
		r.exact.Settle(actual)
	}
	return lua.NoWrites{}
}

// Release takes back the request counts counted here and releases the exact counters.
func (r *reservation) Release() {
	r.undoLocal()
	if r.exact != nil {
		r.exact.Release()
	}
}

func (l *Limiter) settleLocal(r *reservation, actual limits.Usage) {
	now := l.clock()
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := range r.local {
		h := &r.local[i]
		c := &r.counters[h.counter]
		rule, err := lua.Rule(c)
		if err != nil {
			continue
		}
		var charge int64
		switch pkgratelimit.PlainCounterOf(rule) {
		case pkgratelimit.PlainTokens:
			charge = pkgratelimit.TokenCharge(rule.Meter, actual.Tokens)
		case pkgratelimit.PlainSpend:
			charge = actual.CostNanos
		}
		if charge <= 0 {
			continue
		}
		lk, ok := l.lookupOf(c, rule, now)
		if !ok {
			continue
		}
		lk.counter = h.counter
		s := l.liveSlot(h)
		if s == nil {
			// Found again by identity: a long request may outlive its slot, which the sync frees once idle.
			if s = l.slotOf(r.counters, lk); s == nil {
				continue
			}
		}
		l.chargeIn(s, &lk, charge, now)
	}
}

// chargeIn adds charge to the current bucket of s, lk's slot. Called with mu read-locked.
func (l *Limiter) chargeIn(s *slot, lk *lookup, charge int64, now time.Time) {
	s.mu.RLock()
	if _, ok := l.current(s, lk, now); ok {
		s.cur.delta.Add(charge)
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()
	s.mu.Lock()
	l.touch(s, lk, now)
	s.cur.delta.Add(charge)
	s.mu.Unlock()
}

// undoLocal takes back the request counts Admit added for fixed and sliding windows, in the bucket each went into; a bucket the counter has moved past is no longer read and keeps its count.
func (r *reservation) undoLocal() {
	l := r.lim
	if len(r.local) == 0 {
		return
	}
	now := l.clock()
	l.mu.RLock()
	defer l.mu.RUnlock()
	l.undoHolds(r, now)
}

// undoHolds is undoLocal with mu read-locked.
func (l *Limiter) undoHolds(r *reservation, now time.Time) {
	for i := range r.local {
		h := &r.local[i]
		if s := l.liveSlot(h); s != nil {
			if countsRequests(s.kind) {
				s.mu.RLock()
				if bk := s.bucket(h.bucket); bk != nil {
					bk.delta.Add(-1)
				}
				s.mu.RUnlock()
			}
			continue
		}
		c := &r.counters[h.counter]
		rule, err := lua.Rule(c)
		if err != nil {
			continue
		}
		lk, ok := l.lookupOf(c, rule, now)
		if !ok {
			continue
		}
		lk.counter = h.counter
		s := l.slotOf(r.counters, lk)
		if s == nil {
			continue
		}
		s.mu.Lock()
		l.touch(s, &lk, now)
		if countsRequests(s.kind) {
			if bk := s.bucket(h.bucket); bk != nil {
				bk.delta.Add(-1)
			}
		}
		s.mu.Unlock()
	}
}

// liveSlot returns h's slot while it still counts h's counter, and nil once the sync has freed it. Called with mu read-locked.
func (l *Limiter) liveSlot(h *hold) *slot {
	if s := &l.tab.slots[h.slot]; s.live && s.gen == h.gen {
		return s
	}
	return nil
}

// slotOf returns lk's slot, adding it when the table has none, and nil when lk's counter cannot be counted here. Called with mu read-locked, which it may drop and retake.
func (l *Limiter) slotOf(cs []limits.Counter, lk lookup) *slot {
	var none [1]int
	lks, _ := l.findSlots(cs, []lookup{lk}, none[:0])
	if len(lks) == 0 {
		return nil
	}
	return &l.tab.slots[lks[0].slot]
}
