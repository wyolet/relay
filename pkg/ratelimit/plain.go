package ratelimit

import (
	"errors"
	"math"
	"time"
)

// PlainCounter names how a rule counts when its whole state is one store counter per window bucket or calendar period. Only such rules can be counted elsewhere and added to the store as deltas: the reserve script reads them with GET and the commit script writes them with INCRBY.
type PlainCounter uint8

const (
	NotPlain PlainCounter = iota
	PlainFixedWindow
	PlainSlidingWindow
	PlainTokens
	PlainSpend
)

var errNotPlain = errors.New("limit: rule keeps no plain counter")

// PlainCounterOf reports how r counts: token buckets, leaky buckets, session windows, concurrency leases and revocation checks are NotPlain.
func PlainCounterOf(r Rule) PlainCounter {
	switch {
	case r.Meter == MeterSpend:
		return PlainSpend
	case r.Meter == MeterRevoked || r.Meter == "concurrency":
		return NotPlain
	case isTokenMeter(r.Meter):
		return PlainTokens
	case r.Strategy == StrategyFixedWindow:
		return PlainFixedWindow
	case r.Strategy == StrategySlidingWindow:
		return PlainSlidingWindow
	}
	return NotPlain
}

// BucketStart returns the start of the window bucket or calendar period r counts in at t, the one the reserve script would read at t.
func BucketStart(r Rule, t time.Time) (time.Time, error) {
	switch PlainCounterOf(r) {
	case PlainFixedWindow:
		return time.UnixMilli(fixedWindowStartMs(t, r.Window)), nil
	case PlainSlidingWindow, PlainTokens:
		cur, _ := windowBuckets(t, r.Window)
		return cur, nil
	case PlainSpend:
		start, _, err := periodBounds(r.Period, t)
		return start, err
	}
	return time.Time{}, errNotPlain
}

// CounterKey returns the store key of r's counter for the bucket starting at start, and the TTL a write at now gives it when it has none. A TTL of zero or less means the bucket is too old to be read by anyone.
func CounterKey(scope string, r Rule, start, now time.Time) (string, time.Duration, error) {
	switch PlainCounterOf(r) {
	case PlainFixedWindow:
		return fixedWindowKey(scope, r, start.UnixMilli()), 2 * r.Window, nil
	case PlainSlidingWindow, PlainTokens:
		return bucketKey(scope, r, start), 2 * r.Window, nil
	case PlainSpend:
		_, end, err := periodBounds(r.Period, start)
		if err != nil {
			return "", 0, err
		}
		return spendKey(scope, r, start), end.Add(spendKeyGrace).Sub(now), nil
	}
	return "", 0, errNotPlain
}

// PlainCount is the count the reserve script compares with r's amount at now, before the request's own: the current bucket for fixed windows and spend, the weighted two-bucket count for sliding windows and token meters.
func PlainCount(r Rule, cur, prev int64, now time.Time) float64 {
	switch PlainCounterOf(r) {
	case PlainSlidingWindow, PlainTokens:
		winMs := r.Window.Milliseconds()
		nowMs := now.UnixMilli()
		frac := float64(nowMs-(nowMs/winMs)*winMs) / float64(winMs)
		return float64(cur) + float64(prev)*(1.0-frac)
	}
	return float64(cur)
}

// CheckPlain decides a reserve of r at now against counts the caller holds, with the reserve script's comparisons and retry-after: cur and prev are the current and previous buckets (prev is unused by fixed windows and spend). It reports whether the request fits and, when it does not, how long until it may.
func CheckPlain(r Rule, cur, prev int64, now time.Time) (time.Duration, bool) {
	nowMs := now.UnixMilli()
	switch PlainCounterOf(r) {
	case PlainSpend:
		if r.Alert || cur < r.Amount {
			return 0, true
		}
		_, end, err := periodBounds(r.Period, now)
		if err != nil {
			return 0, false
		}
		return time.Duration(end.UnixMilli()-nowMs) * time.Millisecond, false
	case PlainFixedWindow:
		winMs := r.Window.Milliseconds()
		if cur+1 <= r.Amount {
			return 0, true
		}
		return time.Duration(winMs-(nowMs-fixedWindowStartMs(now, r.Window))) * time.Millisecond, false
	case PlainTokens:
		if PlainCount(r, cur, prev, now) <= float64(r.Amount) {
			return 0, true
		}
		winMs := r.Window.Milliseconds()
		return time.Duration(winMs-(nowMs-(nowMs/winMs)*winMs)) * time.Millisecond, false
	case PlainSlidingWindow:
		if PlainCount(r, cur+1, prev, now) <= float64(r.Amount) {
			return 0, true
		}
		winMs := r.Window.Milliseconds()
		elapsedMs := nowMs - (nowMs/winMs)*winMs
		retryMs := float64(winMs) - float64(elapsedMs)
		if prev > 0 {
			fracT := 1.0 - float64(r.Amount-cur)/float64(prev)
			if fracT > 0 && fracT < 1.0 {
				if wait := fracT*float64(winMs) - float64(elapsedMs); wait > 0 && wait < retryMs {
					retryMs = wait
				}
			}
		}
		return time.Duration(math.Floor(retryMs)) * time.Millisecond, false
	}
	return 0, false
}

// TokenCharge is what a commit adds to a token meter for tokens: input plus output for the bare meter, the named count for "tokens.<key>". Cache, reasoning and server-tool counts have meters of their own, so summing the whole map would count a request several times.
func TokenCharge(meter string, tokens map[string]int64) int64 {
	if meter == "tokens" {
		return tokens["input"] + tokens["output"]
	}
	if isTokenMeter(meter) {
		return tokens[meter[len("tokens."):]]
	}
	return 0
}
