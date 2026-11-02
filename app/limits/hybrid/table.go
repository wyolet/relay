package hybrid

import (
	"hash/maphash"
	"math"
	"time"

	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// bucket is one window bucket or calendar period of a counter.
type bucket struct {
	start int64 // unix ms
	// global is the store total the latest read returned; readSeq orders reads that may answer out of order.
	global  int64
	readSeq uint32
	// delta is counted here and not yet acknowledged by the store; inflight is the part of it a running push carries.
	delta    int64
	inflight int64
}

func (b *bucket) total() int64 { return b.global + b.delta }

func (b *bucket) pending() int64 { return b.delta - b.inflight }

// slot is one counter's state. It holds no pointers, so the slot slice and the index map are never scanned by the garbage collector; the counter's identity lives in the table's byte arena.
type slot struct {
	hash      uint64
	cur, prev bucket
	amount    int64
	window    time.Duration // window rules; zero for spend
	syncedAt  int64         // unix ns the last sync that read the totals was sent; zero = never read
	touchedAt int64         // unix ns
	// The last sync's view of the current bucket, from which the next one estimates perSync: what the fleet spends in one interval.
	sampleStart int64
	sampleTotal int64
	sampleAt    int64
	perSync     int64
	identOff    uint32
	groupLen    uint16
	idLen       uint16
	meterLen    uint8
	kind        pkgratelimit.PlainCounter
	period      uint8
	rung        uint8
	alert       bool
	exact       bool
	live        bool
	gen         uint32
}

func (s *slot) identLen() int { return int(s.groupLen) + int(s.idLen) + int(s.meterLen) }

func (s *slot) bucket(start int64) *bucket {
	switch start {
	case s.cur.start:
		return &s.cur
	case s.prev.start:
		return &s.prev
	}
	return nil
}

func (s *slot) settled() bool {
	return s.cur.delta == 0 && s.cur.inflight == 0 && s.prev.delta == 0 && s.prev.inflight == 0
}

// shape is the rule s counts, with what bucket and window arithmetic reads and no key.
func (s *slot) shape() pkgratelimit.Rule {
	r := pkgratelimit.Rule{Amount: s.amount, Window: s.window, Period: periods[s.period], Alert: s.alert}
	switch s.kind {
	case pkgratelimit.PlainFixedWindow:
		r.Meter, r.Strategy = "requests", pkgratelimit.StrategyFixedWindow
	case pkgratelimit.PlainSlidingWindow:
		r.Meter, r.Strategy = "requests", pkgratelimit.StrategySlidingWindow
	case pkgratelimit.PlainTokens:
		r.Meter, r.Strategy = "tokens", pkgratelimit.StrategySlidingWindow
	case pkgratelimit.PlainSpend:
		r.Meter = pkgratelimit.MeterSpend
	}
	return r
}

var periods = [...]pkgratelimit.Period{"", pkgratelimit.PeriodDay, pkgratelimit.PeriodWeek, pkgratelimit.PeriodMonth}

func periodCode(p pkgratelimit.Period) uint8 {
	for i, have := range periods {
		if have == p {
			return uint8(i)
		}
	}
	return 0
}

// table maps a counter's identity (group, id, meter) to its slot. Freed slots are reused; the arena only grows until a compaction rewrites it without the bytes of freed slots.
type table struct {
	seed  maphash.Seed
	index map[uint64]uint32
	slots []slot
	free  []uint32
	ident []byte
	dead  int
}

func newTable() table {
	return table{seed: maphash.MakeSeed(), index: make(map[uint64]uint32)}
}

func (t *table) hash(group, id, meter string) uint64 {
	var h maphash.Hash
	h.SetSeed(t.seed)
	_, _ = h.WriteString(group)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(id)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(meter)
	return h.Sum64()
}

// find returns the slot of the identity and whether it has one; collided is set when another identity holds the hash, which leaves this one uncountable here.
func (t *table) find(h uint64, group, id, meter string) (idx uint32, found, collided bool) {
	idx, found = t.index[h]
	if !found {
		return 0, false, false
	}
	s := &t.slots[idx]
	at := int(s.identOff)
	if int(s.groupLen) != len(group) || int(s.idLen) != len(id) || int(s.meterLen) != len(meter) ||
		string(t.ident[at:at+len(group)]) != group ||
		string(t.ident[at+len(group):at+len(group)+len(id)]) != id ||
		string(t.ident[at+len(group)+len(id):at+s.identLen()]) != meter {
		return 0, false, true
	}
	return idx, true, false
}

// add stores a new slot for the identity, or reports false when the identity is too long to store.
func (t *table) add(h uint64, group, id, meter string) (uint32, bool) {
	if len(group) > math.MaxUint16 || len(id) > math.MaxUint16 || len(meter) > math.MaxUint8 || len(t.ident) > math.MaxUint32-len(group)-len(id)-len(meter) {
		return 0, false
	}
	off := uint32(len(t.ident))
	t.ident = append(t.ident, group...)
	t.ident = append(t.ident, id...)
	t.ident = append(t.ident, meter...)
	var idx uint32
	if n := len(t.free); n > 0 {
		idx = t.free[n-1]
		t.free = t.free[:n-1]
	} else {
		t.slots = append(t.slots, slot{})
		idx = uint32(len(t.slots) - 1)
	}
	gen := t.slots[idx].gen
	t.slots[idx] = slot{hash: h, identOff: off, groupLen: uint16(len(group)), idLen: uint16(len(id)), meterLen: uint8(len(meter)), live: true, gen: gen}
	t.index[h] = idx
	return idx, true
}

// remove frees slot idx; holds still naming it see the generation change.
func (t *table) remove(idx uint32) {
	s := &t.slots[idx]
	delete(t.index, s.hash)
	t.dead += s.identLen()
	*s = slot{gen: s.gen + 1}
	t.free = append(t.free, idx)
}

// identity returns s's group, id and meter as new strings. arena is the table's arena as it was while s was live; appends never rewrite bytes already in it, so a sync may read it outside mu.
func identity(arena []byte, s *slot) (group, id, meter string) {
	at := int(s.identOff)
	g, i := at+int(s.groupLen), at+int(s.groupLen)+int(s.idLen)
	return string(arena[at:g]), string(arena[g:i]), string(arena[i : at+s.identLen()])
}

// minCompactBytes keeps a small table from rewriting its arena for a few freed slots.
const minCompactBytes = 64 << 10

// compact rewrites the arena without the bytes of freed slots once they are most of it.
func (t *table) compact() {
	if t.dead < minCompactBytes || 2*t.dead < len(t.ident) {
		return
	}
	arena := make([]byte, 0, len(t.ident)-t.dead)
	for i := range t.slots {
		s := &t.slots[i]
		if !s.live {
			continue
		}
		at := int(s.identOff)
		s.identOff = uint32(len(arena))
		arena = append(arena, t.ident[at:at+s.identLen()]...)
	}
	t.ident, t.dead = arena, 0
}
