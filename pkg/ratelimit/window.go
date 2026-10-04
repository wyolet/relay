package ratelimit

import (
	"time"
)

// windowBuckets returns the current and previous bucket timestamps for t and window W.
func windowBuckets(t time.Time, w time.Duration) (current, previous time.Time) {
	current = t.Truncate(w)
	previous = current.Add(-w)
	return
}
