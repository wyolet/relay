package filter

import (
	"sort"
	"strings"
)

// Apply filters items by the compiled predicates + free-text, sorts the
// matches, then windows by offset/limit. total is the match count BEFORE
// windowing (for "N of M" displays). The input slice is not mutated.
func (q Query[T]) Apply(items []*T) (page []*T, total int) {
	matched := make([]*T, 0, len(items))
	for _, it := range items {
		if it == nil || !q.match(it) {
			continue
		}
		matched = append(matched, it)
	}
	total = len(matched)

	if q.sortField != nil {
		f := q.sortField
		sort.SliceStable(matched, func(i, j int) bool {
			return f.less(matched[i], matched[j]) != q.sortDesc
		})
	}

	lo := q.offset
	if lo > len(matched) {
		lo = len(matched)
	}
	hi := len(matched)
	if q.limit > 0 && lo+q.limit < hi {
		hi = lo + q.limit
	}
	return matched[lo:hi], total
}

func (q Query[T]) match(it *T) bool {
	for _, p := range q.preds {
		if !p(it) {
			return false
		}
	}
	if q.q != "" {
		hit := false
		for _, s := range q.schema.Q(it) {
			if strings.Contains(strings.ToLower(s), q.q) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(q.labels) > 0 {
		have := q.schema.Labels(it)
		for k, v := range q.labels {
			if have[k] != v {
				return false
			}
		}
	}
	return true
}

// less reports whether a sorts before b on this field, ascending.
func (f *Field[T]) less(a, b *T) bool {
	switch f.Kind {
	case Int:
		return f.GetInt(a) < f.GetInt(b)
	case Time:
		return f.GetTime(a).Before(f.GetTime(b))
	case Bool:
		return !f.GetBool(a) && f.GetBool(b)
	default: // String — single-valued only (GetMulti is not orderable)
		return f.Get(a) < f.Get(b)
	}
}
