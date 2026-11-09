package engine

import (
	"hash/maphash"
	"maps"
	"slices"
)

var hashSeed = maphash.MakeSeed()

// shardMap is a map split into partitions. A published shardMap is never
// written again; a shardWriter copies only the partitions a batch touches.
type shardMap[K comparable, V any] struct{ m []map[K]V }

func newShardMap[K comparable, V any](n int) *shardMap[K, V] {
	return &shardMap[K, V]{m: make([]map[K]V, max(n, 1))}
}

func (s *shardMap[K, V]) slot(k K) int {
	if len(s.m) == 1 {
		return 0
	}
	return int(maphash.Comparable(hashSeed, k) % uint64(len(s.m)))
}

func (s *shardMap[K, V]) get(k K) (V, bool) {
	v, ok := s.m[s.slot(k)][k]
	return v, ok
}

func (s *shardMap[K, V]) keys() []K {
	n := 0
	for _, m := range s.m {
		n += len(m)
	}
	out := make([]K, 0, n)
	for _, m := range s.m {
		for k := range m {
			out = append(out, k)
		}
	}
	return out
}

// shardWriter stages one batch of writes over a published shardMap. cur is
// always the working state; freeze hands it out as immutable.
type shardWriter[K comparable, V any] struct {
	cur   *shardMap[K, V]
	owned []bool // nil until the batch's first write
}

func (w *shardWriter[K, V]) get(k K) (V, bool) { return w.cur.get(k) }

func (w *shardWriter[K, V]) set(k K, v V) {
	i := w.own(k)
	w.cur.m[i][k] = v
}

func (w *shardWriter[K, V]) del(k K) {
	if _, ok := w.cur.get(k); !ok {
		return
	}
	i := w.own(k)
	delete(w.cur.m[i], k)
}

func (w *shardWriter[K, V]) own(k K) int {
	if w.owned == nil {
		w.cur = &shardMap[K, V]{m: slices.Clone(w.cur.m)}
		w.owned = make([]bool, len(w.cur.m))
	}
	i := w.cur.slot(k)
	if !w.owned[i] {
		if c := maps.Clone(w.cur.m[i]); c != nil {
			w.cur.m[i] = c
		} else {
			w.cur.m[i] = map[K]V{}
		}
		w.owned[i] = true
	}
	return i
}

func (w *shardWriter[K, V]) freeze() *shardMap[K, V] {
	w.owned = nil
	return w.cur
}
