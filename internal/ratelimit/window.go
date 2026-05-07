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

// readCounter reads an int64 counter from state; missing key = 0.
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

// retryAfterRequests computes how long until the interpolated rate drops below amount.
// If prev is the dominant contributor, we need to wait until the weight of prev falls enough.
// rate = cur + prev*(1-frac) < amount  =>  frac > 1 - (amount-cur)/prev
// remaining elapsed = frac_target * W  =>  wait = frac_target * W - elapsed
func retryAfterRequests(cur, prev, amount int64, t, bucketStart time.Time, w time.Duration) time.Duration {
	// Time until end of current window (always a valid upper bound).
	windowEnd := bucketStart.Add(w)
	maxWait := windowEnd.Sub(t)
	if maxWait < 0 {
		maxWait = 0
	}

	if prev == 0 {
		return maxWait
	}

	// We need: cur + prev*(1-fracTarget) < amount
	// => prev*(1-fracTarget) < amount - cur
	// => 1 - fracTarget < (amount-cur)/prev   (prev > 0)
	// => fracTarget > 1 - (amount-cur)/prev
	need := float64(amount-cur) / float64(prev)
	fracTarget := 1.0 - need
	if fracTarget <= 0 {
		// Already fine after current bucket increments settle — shouldn't happen here.
		return 0
	}
	if fracTarget >= 1 {
		return maxWait
	}
	targetElapsed := time.Duration(fracTarget * float64(w))
	alreadyElapsed := t.Sub(bucketStart)
	wait := targetElapsed - alreadyElapsed
	if wait < 0 {
		wait = 0
	}
	if wait > maxWait {
		wait = maxWait
	}
	return wait
}
