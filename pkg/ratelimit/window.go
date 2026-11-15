package ratelimit

import (
	"math"
	"time"
)

// windowBuckets returns the current and previous bucket timestamps for t and window W.
func windowBuckets(t time.Time, w time.Duration) (current, previous time.Time) {
	current = t.Truncate(w)
	previous = current.Add(-w)
	return
}

// slidingRetryMs is the reserve script's sliding-window retry-after: the whole milliseconds until cur + add + prev's decaying weight is at most amount, add being what the request itself counts. When cur + add alone is over, that falls in the next window, where cur weighs in as the previous bucket.
func slidingRetryMs(amount, cur, prev, add, elapsedMs, winMs int64) int64 {
	retryMs := float64(winMs - elapsedMs)
	switch {
	case cur+add > amount && cur > 0:
		retryMs = retryMs + float64(winMs) - float64(amount-add)*float64(winMs)/float64(cur)
	case prev > 0:
		if wait := retryMs - float64(amount-cur-add)*float64(winMs)/float64(prev); wait > 0 && wait < retryMs {
			retryMs = wait
		}
	}
	return int64(math.Ceil(retryMs))
}
