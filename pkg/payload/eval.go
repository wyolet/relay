package payload

import (
	"sort"
	"time"
)

// In-memory query evaluation shared by scan-style backends (file, s3) that
// pull a candidate set of Records and filter/sort/paginate in Go. Mirrors
// sdk/usage/eval.go for the narrower Record shape.

// FilterRecords returns the subset matching q's time cutoff, dimension
// filters, status range, and keyset cursor. It does not sort or apply
// Limit; use SortAndLimit for that.
func FilterRecords(records []Record, q Query) []Record {
	var cutoff time.Time
	switch {
	case !q.From.IsZero():
		cutoff = q.From
	case q.Since > 0:
		cutoff = time.Now().Add(-q.Since)
	}
	out := records[:0:0]
	for _, r := range records {
		if matches(r, q, cutoff) {
			out = append(out, r)
		}
	}
	return out
}

func matches(r Record, q Query, cutoff time.Time) bool {
	if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
		return false
	}
	if !q.To.IsZero() && r.Timestamp.After(q.To) {
		return false
	}
	if q.StatusMin > 0 && r.Status < q.StatusMin {
		return false
	}
	if q.StatusMax > 0 && r.Status > q.StatusMax {
		return false
	}
	if !inSet(q.RelayKeyHash, r.RelayKeyHash) ||
		!inSet(q.PolicyID, r.PolicyID) ||
		!inSet(q.ModelID, r.ModelID) ||
		!inSet(q.HostID, r.HostID) ||
		!inSet(q.Source, r.Source) ||
		!inSet(q.ErrorKind, r.ErrorKind) {
		return false
	}
	// Keyset cursor: strictly older than (CursorTS, CursorID) under the
	// (ts DESC, request_id DESC) ordering.
	if !q.CursorTS.IsZero() {
		if r.Timestamp.After(q.CursorTS) {
			return false
		}
		if r.Timestamp.Equal(q.CursorTS) && r.RequestID >= q.CursorID {
			return false
		}
	}
	return true
}

// inSet reports whether v is in want, treating an empty want as "match
// any" (no filter on this dimension).
func inSet(want []string, v string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if w == v {
			return true
		}
	}
	return false
}

// SortAndLimit sorts records newest-first and caps to limit (clamped to
// [DefaultLimit, MaxLimit] when out of range).
func SortAndLimit(records []Record, limit int) []Record {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].Timestamp.Equal(records[j].Timestamp) {
			return records[i].Timestamp.After(records[j].Timestamp)
		}
		return records[i].RequestID > records[j].RequestID // stable keyset tiebreak
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records
}

// StripBody returns r with the request/response bodies cleared, keeping the
// metadata + truncation flags. Used by Reader.List so the table view never
// ships captured bodies.
func StripBody(r Record) Record {
	r.RequestBody = nil
	r.ResponseBody = nil
	return r
}
