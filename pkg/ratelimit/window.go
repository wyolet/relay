package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// windowBuckets returns the current and previous bucket timestamps for t and window W.
func windowBuckets(t time.Time, w time.Duration) (current, previous time.Time) {
	current = t.Truncate(w)
	previous = current.Add(-w)
	return
}

// fractionElapsed returns how far we are into the current bucket [0,1].
func fractionElapsed(t time.Time, current time.Time, w time.Duration) float64 {
	f := t.Sub(current).Seconds() / w.Seconds()
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// readCounter reads an int64 counter from store; missing key = 0.
func readCounter(ctx context.Context, st kv.Store, key string) (int64, error) {
	b, err := st.Get(ctx, key)
	if err != nil {
		if errors.Is(err, kv.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// interpolatedRate computes the sliding-window rate.
func interpolatedRate(cur, prev int64, frac float64) float64 {
	return float64(cur) + float64(prev)*(1-frac)
}
