package usage

import (
	"sort"
	"strings"
	"time"
)

// In-memory query evaluation shared by scan-style backends (file, valkey)
// that pull a candidate set of events and filter/aggregate in Go rather
// than pushing the query into a store engine. The ClickHouse backend does
// NOT use these — it pushes filters + aggregation into SQL.

// FilterEvents returns the subset of events matching q's time cutoff,
// dimension filters, and status range. It does not sort or apply Limit;
// use SortAndLimit for that.
func FilterEvents(events []Event, q EventQuery) []Event {
	cutoff := lowerBound(q)
	out := events[:0:0]
	for _, ev := range events {
		if matches(ev, q, cutoff) {
			out = append(out, ev)
		}
	}
	return out
}

// lowerBound resolves q's lower time bound: From, else now-Since, else zero (unbounded).
func lowerBound(q EventQuery) time.Time {
	switch {
	case !q.From.IsZero():
		return q.From
	case q.Since > 0:
		return time.Now().Add(-q.Since)
	}
	return time.Time{}
}

// SortAndLimit sorts events newest-first and caps to limit (clamped to
// [DefaultEventLimit, MaxEventLimit] when out of range).
func SortAndLimit(events []Event, limit int) []Event {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.After(events[j].Timestamp)
		}
		return events[i].RequestID > events[j].RequestID // stable keyset tiebreak
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events
}

func matches(ev Event, q EventQuery, cutoff time.Time) bool {
	// Lower bound: cutoff (From, or now-Since — resolved by caller).
	if !cutoff.IsZero() && ev.Timestamp.Before(cutoff) {
		return false
	}
	if !q.To.IsZero() && ev.Timestamp.After(q.To) {
		return false
	}
	if !q.Before.IsZero() && !ev.Timestamp.Before(q.Before) {
		return false
	}
	// Keyset cursor: strictly older than (CursorTS, CursorID) under
	// (ts DESC, request_id DESC).
	if !q.CursorTS.IsZero() {
		if ev.Timestamp.After(q.CursorTS) {
			return false
		}
		if ev.Timestamp.Equal(q.CursorTS) && ev.RequestID >= q.CursorID {
			return false
		}
	}
	if q.RequestID != "" && ev.RequestID != q.RequestID {
		return false
	}
	if !inScope(q, ev) {
		return false
	}
	if !inList(q.RelayKeyHash, ev.RelayKeyHash) {
		return false
	}
	if !inList(q.PolicyID, ev.PolicyID) {
		return false
	}
	if !inList(q.ProjectID, ev.ProjectID) {
		return false
	}
	if !inList(q.TeamID, ev.TeamID) {
		return false
	}
	if !inList(q.PrincipalID, ev.PrincipalID) {
		return false
	}
	if !inList(q.ModelID, ev.ModelID) {
		return false
	}
	if !inList(q.HostID, ev.HostID) {
		return false
	}
	if !inList(q.Source, ev.Source) {
		return false
	}
	if !inList(q.FinishReason, ev.FinishReason) {
		return false
	}
	if !inList(q.ErrorKind, ev.ErrorKind) {
		return false
	}
	if q.StatusMin > 0 && ev.Status < q.StatusMin {
		return false
	}
	if q.StatusMax > 0 && ev.Status > q.StatusMax {
		return false
	}
	if len(q.Status) > 0 && !intInList(q.Status, ev.Status) {
		return false
	}
	if !inList(q.HostKeyID, ev.HostKeyID) {
		return false
	}
	if !inList(q.RequestedModel, ev.RequestedModel) {
		return false
	}
	if !inList(q.Model, ev.Model) {
		return false
	}
	if !inList(q.Host, ev.Host) {
		return false
	}
	if !inList(q.Policy, ev.Policy) {
		return false
	}
	if !inList(q.Provider, ev.Provider) {
		return false
	}
	if !inList(q.RelayKey, ev.RelayKey) {
		return false
	}
	for k, vals := range q.Tags {
		if !inList(vals, ev.Tags[k]) {
			return false
		}
	}
	if q.Streamed != nil && ev.Streamed != *q.Streamed {
		return false
	}
	if q.ErrorsOnly != nil {
		isErr := ev.Status >= 400 || ev.ErrorKind != ""
		if isErr != *q.ErrorsOnly {
			return false
		}
	}
	if q.AttemptsMin > 0 && ev.Attempts < q.AttemptsMin {
		return false
	}
	if q.DurationMsMin > 0 && ev.DurationMs < q.DurationMsMin {
		return false
	}
	if q.DurationMsMax > 0 && ev.DurationMs > q.DurationMsMax {
		return false
	}
	if q.TTFTMsMin > 0 || q.TTFTMsMax > 0 {
		if ev.Upstream == nil {
			return false
		}
		ttft := ev.Upstream.ResponseStart / 1000 // µs → ms
		if q.TTFTMsMin > 0 && ttft < q.TTFTMsMin {
			return false
		}
		if q.TTFTMsMax > 0 && ttft > q.TTFTMsMax {
			return false
		}
	}
	if q.Q != "" {
		needle := strings.ToLower(q.Q)
		if !containsFold(ev.RequestID, needle) &&
			!containsFold(ev.ModelID, needle) &&
			!containsFold(ev.RequestedModel, needle) &&
			!containsFold(ev.Source, needle) {
			return false
		}
	}
	return true
}

func intInList(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains needle (needle already lowercased).
func containsFold(s, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(s), lowerNeedle)
}

// inList reports whether val is in set, treating an empty set as "no
// filter" (matches everything).
func inList(set []string, val string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == val {
			return true
		}
	}
	return false
}

// inScope applies the read-scope disjunction: within the caller's projects
// OR from one of the caller's own keys. No scope set means no narrowing.
func inScope(q EventQuery, ev Event) bool {
	if len(q.ScopeProjectID) == 0 && len(q.ScopeRelayKeyHash) == 0 && len(q.ScopePrincipalID) == 0 {
		return true
	}
	for _, p := range q.ScopeProjectID {
		if p != "" && p == ev.ProjectID {
			return true
		}
	}
	for _, h := range q.ScopeRelayKeyHash {
		if h != "" && h == ev.RelayKeyHash {
			return true
		}
	}
	for _, p := range q.ScopePrincipalID {
		if p != "" && p == ev.PrincipalID {
			return true
		}
	}
	return false
}
