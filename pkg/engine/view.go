package engine

import "slices"

// View is one published generation: every object, the recorded graph and the
// issues. Immutable once published; object pointers taken from any view stay
// valid forever.
type View struct {
	gen    Generation
	sch    *schema
	kinds  []objects
	refs   *shardMap[Edge, refSet]
	issues *shardMap[Ref, map[issueOwner][]Issue]
}

func emptyView(sch *schema) *View {
	return &View{
		sch:    sch,
		refs:   newShardMap[Edge, refSet](1),
		issues: newShardMap[Ref, map[issueOwner][]Issue](1),
	}
}

// Gen is the generation this view was published as.
func (v *View) Gen() Generation { return v.gen }

func (v *View) objects(k *kindInfo) objects {
	if k.idx >= len(v.kinds) {
		return objects{}
	}
	return v.kinds[k.idx]
}

// Issues returns the issues attached to ref by any controller or the engine,
// sorted by severity then code.
func (v *View) Issues(ref Ref) []Issue {
	m, _ := v.issues.get(ref)
	var out []Issue
	for _, list := range m {
		out = append(out, list...)
	}
	slices.SortFunc(out, cmpIssue)
	return out
}

// AllIssues returns every issue, ordered by the Ref it is attached to, then
// severity and code.
func (v *View) AllIssues() []Issue {
	var out []Issue
	for _, m := range v.issues.m {
		for _, owners := range m {
			for _, list := range owners {
				out = append(out, list...)
			}
		}
	}
	slices.SortFunc(out, func(a, b Issue) int {
		if c := cmpRef(a.Source, b.Source); c != 0 {
			return c
		}
		return cmpIssue(a, b)
	})
	return out
}

// Referrers returns every object whose last reconcile recorded e, sorted. An
// Any edge answers nothing useful and returns nil.
func (v *View) Referrers(e Edge) []Ref {
	if e.all {
		return nil
	}
	s, _ := v.refs.get(e)
	if len(s) == 0 {
		return nil
	}
	out := make([]Ref, 0, len(s))
	for r := range s {
		out = append(out, r)
	}
	slices.SortFunc(out, cmpRef)
	return out
}
