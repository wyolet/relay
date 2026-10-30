package engine

import (
	"maps"
	"slices"
)

type refSet = map[Ref]struct{}

// graph is the reverse index of recorded edges: edge → the outputs whose last
// reconcile recorded it. Sets are copied once per batch, then edited in place.
type graph struct {
	w     shardWriter[Edge, refSet]
	owned map[Edge]bool
}

func newGraph() graph {
	return graph{w: shardWriter[Edge, refSet]{cur: newShardMap[Edge, refSet](256)}, owned: map[Edge]bool{}}
}

func (g *graph) referrers(e Edge) refSet {
	s, _ := g.w.get(e)
	return s
}

func (g *graph) add(e Edge, r Ref) {
	s, _ := g.w.get(e)
	if !g.owned[e] {
		c := make(refSet, len(s)+1)
		maps.Copy(c, s)
		s = c
		g.w.set(e, s)
		g.owned[e] = true
	}
	s[r] = struct{}{}
}

func (g *graph) remove(e Edge, r Ref) {
	s, _ := g.w.get(e)
	if _, ok := s[r]; !ok {
		return
	}
	if len(s) == 1 {
		g.w.del(e)
		delete(g.owned, e)
		return
	}
	if !g.owned[e] {
		s = maps.Clone(s)
		g.w.set(e, s)
		g.owned[e] = true
	}
	delete(s, r)
}

func (g *graph) freeze() *shardMap[Edge, refSet] {
	clear(g.owned)
	return g.w.freeze()
}

// normalize sorts and deduplicates a recorded slice in place.
func normalize[T comparable](s []T, cmp func(a, b T) int) []T {
	slices.SortFunc(s, cmp)
	return slices.Compact(s)
}

// diffSorted calls removed for elements only in old and added for elements
// only in cur; both slices are sorted by cmp and duplicate-free.
func diffSorted[T any](old, cur []T, cmp func(a, b T) int, removed, added func(T)) {
	i, j := 0, 0
	for i < len(old) || j < len(cur) {
		switch {
		case j == len(cur) || (i < len(old) && cmp(old[i], cur[j]) < 0):
			removed(old[i])
			i++
		case i == len(old) || cmp(old[i], cur[j]) > 0:
			added(cur[j])
			j++
		default:
			i++
			j++
		}
	}
}
