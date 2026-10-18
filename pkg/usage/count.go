package usage

import "time"

// CountEvents counts the events FilterEvents would keep for q, ignoring q's cursor, and stops past limit (<= 0 counts all). It is Reader.Count for the scan-style backends.
func CountEvents(events []Event, q EventQuery, limit int) int64 {
	q.CursorTS, q.CursorID = time.Time{}, ""
	cutoff := lowerBound(q)
	var n int64
	for _, ev := range events {
		if !matches(ev, q, cutoff) {
			continue
		}
		n++
		if limit > 0 && n >= int64(limit) {
			break
		}
	}
	return n
}
