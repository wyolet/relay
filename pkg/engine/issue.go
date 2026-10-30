package engine

import (
	"cmp"
	"errors"
	"maps"
	"slices"
)

// Severity ranks an Issue.
type Severity uint8

const (
	SeverityError   Severity = iota + 1 // the object cannot serve
	SeverityWarning                     // it serves, something is probably wrong
)

// Issue is one diagnostic attached to an object. Code is a stable snake_case
// id the UI keys on; Field is the spec path; Target is what Source pointed at
// when it broke.
type Issue struct {
	Severity Severity
	Code     string
	Source   Ref
	Target   Ref
	Field    string
	Message  string
}

// ErrRetry marks a transient reconcile failure: retried on the next drain
// without backoff.
var ErrRetry = errors.New("engine: retry")

var errNoOutput = errors.New("engine: reconcile returned without Put or Drop")

func cmpIssue(a, b Issue) int {
	return cmp.Or(cmp.Compare(a.Severity, b.Severity), cmp.Compare(a.Code, b.Code),
		cmpRef(a.Source, b.Source), cmpRef(a.Target, b.Target),
		cmp.Compare(a.Field, b.Field), cmp.Compare(a.Message, b.Message))
}

// issueOwner is who attached a set of issues: a controller's reconcile of one
// output, or the engine itself on that output's behalf.
type issueOwner struct {
	by  ownerKind
	ctl string
	out Ref
}

type ownerKind uint8

const (
	byController ownerKind = iota
	byDuplicateName
	byReconcileFailure
	byAbandonedBatch
)

// issueStore indexes issue sets by the Ref they are attached to, so View.Issues
// is one lookup; byOwner is the write side's record of what each owner set.
type issueStore struct {
	w       shardWriter[Ref, map[issueOwner][]Issue]
	byOwner map[issueOwner][]Issue
}

func newIssueStore() issueStore {
	return issueStore{
		w:       shardWriter[Ref, map[issueOwner][]Issue]{cur: newShardMap[Ref, map[issueOwner][]Issue](64)},
		byOwner: map[issueOwner][]Issue{},
	}
}

// set replaces o's issue set and reports whether it changed.
func (s *issueStore) set(o issueOwner, list []Issue) bool {
	old := s.byOwner[o]
	if slices.Equal(old, list) {
		return false
	}
	for _, src := range sources(old) {
		m, _ := s.w.get(src)
		m = maps.Clone(m)
		delete(m, o)
		if len(m) == 0 {
			s.w.del(src)
		} else {
			s.w.set(src, m)
		}
	}
	for _, src := range sources(list) {
		m, _ := s.w.get(src)
		m = maps.Clone(m)
		if m == nil {
			m = map[issueOwner][]Issue{}
		}
		var own []Issue
		for _, i := range list {
			if i.Source == src {
				own = append(own, i)
			}
		}
		m[o] = own
		s.w.set(src, m)
	}
	if len(list) == 0 {
		delete(s.byOwner, o)
	} else {
		s.byOwner[o] = slices.Clone(list)
	}
	return true
}

func sources(list []Issue) []Ref {
	var out []Ref
	for _, i := range list {
		if !slices.Contains(out, i.Source) {
			out = append(out, i.Source)
		}
	}
	return out
}
