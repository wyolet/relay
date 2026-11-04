package hybrid

import (
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
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range r.local {
		c := &r.counters[r.local[i].counter]
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
		// Found again by identity: a long request may outlive its slot, which the sync frees once idle.
		if idx, ok := l.localSlot(c, rule, now); ok {
			l.tab.slots[idx].cur.delta += charge
		}
	}
}

// undoLocal takes back the request counts Admit added for fixed and sliding windows, in the bucket each went into; a bucket the counter has moved past is no longer read and keeps its count.
func (r *reservation) undoLocal() {
	l := r.lim
	if len(r.local) == 0 {
		return
	}
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range r.local {
		h := &r.local[i]
		s := &l.tab.slots[h.slot]
		if !s.live || s.gen != h.gen {
			c := &r.counters[h.counter]
			rule, err := lua.Rule(c)
			if err != nil {
				continue
			}
			idx, ok := l.localSlot(c, rule, now)
			if !ok {
				continue
			}
			s = &l.tab.slots[idx]
		}
		if !countsRequests(s.kind) {
			continue
		}
		if bk := s.bucket(h.bucket); bk != nil {
			bk.delta--
		}
	}
}
