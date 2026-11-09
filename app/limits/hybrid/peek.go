package hybrid

import (
	"context"
	"math"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Peek reads a counter this pod holds a synced slot for from memory: the fleet total its last sync read plus this pod's unsynced count, so it lags what other pods and exact admissions counted since by up to one sync interval. Every other counter is read from the store by the Lua limiter's Peek, which holds each pod's local counts as of its last sync. Nothing is counted and the table is left as it was.
func (l *Limiter) Peek(ctx context.Context, cs []limits.Counter) ([]limits.Reading, error) {
	out := make([]limits.Reading, len(cs))
	now := l.clock()
	var (
		stored []limits.Counter
		at     []int
	)
	for i := range cs {
		c := &cs[i]
		r, err := lua.Rule(c)
		if err != nil {
			return nil, err
		}
		out[i].Amount = r.Amount
		if used, ok := l.localCount(c, r, now); ok {
			out[i].Used = used
			continue
		}
		stored, at = append(stored, *c), append(at, i)
	}
	if len(stored) == 0 {
		return out, nil
	}
	read, err := l.exact.Peek(ctx, stored)
	if err != nil {
		return nil, err
	}
	for j, i := range at {
		out[i].Used = read[j].Used
	}
	return out, nil
}

// localCount is c's count at now from its slot, without moving the slot to now's bucket; false when the slot is missing, never read, or too far behind now to know the previous bucket.
func (l *Limiter) localCount(c *limits.Counter, r pkgratelimit.Rule, now time.Time) (int64, bool) {
	if pkgratelimit.PlainCounterOf(r) == pkgratelimit.NotPlain {
		return 0, false
	}
	h := l.tab.hash(c.Group, c.ID, r.Meter)
	l.mu.RLock()
	defer l.mu.RUnlock()
	idx, found, _ := l.tab.find(h, c.Group, c.ID, r.Meter)
	if !found {
		return 0, false
	}
	s := &l.tab.slots[idx]
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.syncedAt == 0 {
		return 0, false
	}
	shape := s.shape()
	t, err := pkgratelimit.BucketStart(shape, now)
	if err != nil {
		return 0, false
	}
	cur, prev := s.cur.total(), s.prev.total()
	if start := t.UnixMilli(); start != s.cur.start {
		if previousStart(shape, start) != s.cur.start {
			return 0, false
		}
		cur, prev = 0, cur
	}
	return int64(math.Ceil(pkgratelimit.PlainCount(shape, cur, prev, now) - 1e-9)), true
}
