package hybrid

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// push is one bucket's unsynced count on its way to the store, or a read of the bucket's total when read is set.
type push struct {
	slot, gen uint32
	// ref is the request's counter the bucket belongs to (Admit) or the sync target (sync).
	ref    int
	start  int64
	count  int64
	seq    uint32
	read   bool
	result *kv.BatchResult // nil when nothing was sent
}

// takePending appends a push for every bucket of slot idx with an unsynced count not already on its way, and marks that count in flight. Called with the slot write-locked.
func (l *Limiter) takePending(pushes []push, idx uint32, ref int) []push {
	s := &l.tab.slots[idx]
	for _, bk := range [2]*bucket{&s.cur, &s.prev} {
		if n := bk.pending(); n != 0 {
			s.seq++
			pushes = append(pushes, push{slot: idx, gen: s.gen, ref: ref, start: bk.start, count: n, seq: s.seq})
			bk.inflight += n
		}
	}
	return pushes
}

// queuePushes queues each push on b: INCRBY and EXPIRE NX for a count, GET for a read. name returns the push's scope and rule; a push it cannot name, or whose bucket is too old to keep, is not sent.
func queuePushes(b *kv.Batch, pushes []push, name func(*push) (string, pkgratelimit.Rule, bool), now time.Time) {
	for i := range pushes {
		p := &pushes[i]
		scope, r, ok := name(p)
		if !ok {
			continue
		}
		key, ttl, err := counterKey(scope, r, p.start, now)
		if err != nil || ttl <= 0 {
			continue
		}
		if p.read {
			p.result = b.Get(key)
			continue
		}
		p.result = b.Incr(key, p.count)
		b.ExpireNX(key, ttl)
	}
}

// ackPushes applies the outcome of pushes; ran is false when their batch never ran.
func (l *Limiter) ackPushes(pushes []push, ran bool) {
	if len(pushes) == 0 {
		return
	}
	l.mu.RLock()
	for i := range pushes {
		l.applyPush(&pushes[i], ran)
	}
	l.mu.RUnlock()
}

// applyPush folds one push's outcome into its slot and reports whether the store failed it. A count leaves the slot only once the store acknowledged it, so an unanswered one is sent again; one whose bucket was too old to send is dropped. Called with mu read-locked.
func (l *Limiter) applyPush(p *push, ran bool) (failed bool) {
	s := &l.tab.slots[p.slot]
	if !s.live || s.gen != p.gen {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bk := s.bucket(p.start)
	if bk == nil {
		return false
	}
	if !p.read {
		bk.inflight -= p.count
	}
	if !ran {
		return true
	}
	if p.result == nil {
		if !p.read {
			bk.delta.Add(-p.count)
		}
		return false
	}
	total, err := p.result.Int, p.result.Err
	if p.read {
		total, err = readTotal(p.result)
	}
	if err != nil {
		return true
	}
	if !p.read {
		bk.delta.Add(-p.count)
		if s.kind == pkgratelimit.PlainSpend && p.count > 0 && total >= s.amount && total-p.count < s.amount {
			lua.CountBudgetCrossing(rungs[s.rung], budgetMode(s.alert))
		}
	}
	if p.seq > bk.readSeq {
		bk.global, bk.readSeq = total, p.seq
	}
	return false
}

func readTotal(r *kv.BatchResult) (int64, error) {
	if errors.Is(r.Err, kv.ErrNotFound) {
		return 0, nil
	}
	if r.Err != nil {
		return 0, r.Err
	}
	return strconv.ParseInt(string(r.Value), 10, 64)
}

// target is one slot a sync covers, with what naming its keys needs, copied so the keys are built outside mu.
type target struct {
	slot, gen uint32
	identOff  uint32
	groupLen  uint16
	idLen     uint16
	meterLen  uint8
	shape     pkgratelimit.Rule
	read      bool
	failed    bool
}

func (t *target) rule(arena []byte) (string, pkgratelimit.Rule) {
	s := slot{identOff: t.identOff, groupLen: t.groupLen, idLen: t.idLen, meterLen: t.meterLen}
	group, id, meter := identity(arena, &s)
	r := t.shape
	r.Key, r.Meter = id, meter
	return group, r
}

// sync adds every unsynced count to the store and reads back the totals of the counters touched since the last sync, decides each one's mode and drops idle counters. Syncs run one at a time.
func (l *Limiter) sync() {
	l.syncing.Lock()
	defer l.syncing.Unlock()
	sent := l.clock()
	since := l.lastSync
	l.lastSync = sent.UnixNano()
	var (
		targets []target
		pushes  []push
	)
	// Each slot is read and its pending counts marked in flight under its write lock, so a count added after is pending for the next sync, and one added before rides this one.
	l.mu.RLock()
	for i := range l.tab.slots {
		s := &l.tab.slots[i]
		if !s.live {
			continue
		}
		s.mu.Lock()
		targets, pushes = l.collect(s, uint32(i), since, sent, targets, pushes)
		s.mu.Unlock()
	}
	arena := l.tab.ident
	l.mu.RUnlock()

	ran := false
	if len(pushes) > 0 {
		b := kv.NewBatch(l.syncStore, 2*len(pushes))
		queuePushes(b, pushes, func(p *push) (string, pkgratelimit.Rule, bool) {
			group, r := targets[p.ref].rule(arena)
			return group, r, true
		}, sent)
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		b.Exec(ctx)
		cancel()
		ran = true
	}

	l.mu.RLock()
	failed := false
	for i := range pushes {
		if l.applyPush(&pushes[i], ran) {
			targets[pushes[i].ref].failed = true
			failed = true
		}
	}
	if ran {
		syncOutcome(failed)
	}
	for i := range targets {
		t := &targets[i]
		s := &l.tab.slots[t.slot]
		if !t.read || t.failed || !s.live || s.gen != t.gen {
			continue
		}
		s.mu.Lock()
		s.syncedAt = sent.UnixNano()
		l.sample(s, sent)
		s.exact = l.nearCap(s, sent)
		s.mu.Unlock()
	}
	l.mu.RUnlock()
	l.dropIdle(sent)
}

// collect appends to targets and pushes what the sync at sent covers of slot idx, s: its pending counts and, when a request touched it since the last sync, reads of its totals. Called with s write-locked.
func (l *Limiter) collect(s *slot, idx uint32, since int64, sent time.Time, targets []target, pushes []push) ([]target, []push) {
	touched := s.touchedAt.Load() >= since
	if !touched && s.cur.pending() == 0 && s.prev.pending() == 0 {
		return targets, pushes
	}
	l.rollTo(s, sent)
	ti := len(targets)
	targets = append(targets, target{
		slot: idx, gen: s.gen, identOff: s.identOff, groupLen: s.groupLen, idLen: s.idLen, meterLen: s.meterLen,
		shape: s.shape(), read: touched,
	})
	pushes = l.takePending(pushes, idx, ti)
	if !touched {
		return targets, pushes
	}
	// Reads cover the buckets the checks use; a bucket with a push in flight gets its total from that push.
	n := 1
	if s.kind == pkgratelimit.PlainSlidingWindow || s.kind == pkgratelimit.PlainTokens {
		n = 2
	}
	read := [2]*bucket{&s.cur, &s.prev}
	for _, bk := range read[:n] {
		if bk.inflight == 0 {
			s.seq++
			pushes = append(pushes, push{slot: idx, gen: s.gen, ref: ti, start: bk.start, seq: s.seq, read: true})
		}
	}
	return targets, pushes
}

// sample estimates what the fleet spends on s in one interval from the growth of its current bucket since the previous sync, or since the bucket started.
func (l *Limiter) sample(s *slot, now time.Time) {
	nowNs := now.UnixNano()
	total := s.cur.total()
	grown, elapsed := total, nowNs-s.cur.start*int64(time.Millisecond)
	if s.sampleAt != 0 && s.sampleStart == s.cur.start {
		grown, elapsed = total-s.sampleTotal, nowNs-s.sampleAt
	}
	// A floor on the elapsed time keeps a sync just after a bucket starts from reading a few counts as a huge rate.
	elapsed = max(elapsed, int64(l.interval)/10)
	s.perSync = int64(float64(max(grown, 0)) * float64(l.interval) / float64(elapsed))
	s.sampleStart, s.sampleTotal, s.sampleAt = s.cur.start, total, nowNs
}

// dropIdle drops counters untouched for idleAfter whose counts are all in the store, compacts the arena, and reports the modes. The table is write-locked only when there is something to drop.
func (l *Limiter) dropIdle(now time.Time) {
	var local, exact int
	var idle []uint32
	l.mu.RLock()
	for i := range l.tab.slots {
		s := &l.tab.slots[i]
		if !s.live {
			continue
		}
		s.mu.RLock()
		switch {
		case l.idle(s, now):
			idle = append(idle, uint32(i))
		case l.usable(s, now):
			local++
		default:
			exact++
		}
		s.mu.RUnlock()
	}
	l.mu.RUnlock()
	if len(idle) > 0 {
		l.mu.Lock()
		for _, idx := range idle {
			// A request may have touched it since the scan.
			switch s := &l.tab.slots[idx]; {
			case !s.live:
			case l.idle(s, now):
				l.tab.remove(idx)
			case l.usable(s, now):
				local++
			default:
				exact++
			}
		}
		l.tab.compact()
		l.mu.Unlock()
	}
	reportModes(local, exact)
}

// idle reports whether s has gone untouched for idleAfter with all its counts in the store.
func (l *Limiter) idle(s *slot, now time.Time) bool {
	return now.UnixNano()-s.touchedAt.Load() > int64(idleAfter) && s.settled()
}
