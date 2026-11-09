package ratelimit

import (
	"strconv"
	"time"
)

// keyBufSize fits the longest usual key on the stack (a per-model policy rule with UUID ids, ~280 bytes), so building it allocates only the returned string.
const keyBufSize = 320

// bucketKey returns the state key for a sliding-window bucket.
// format: limit:{<scope>}:<rule.Key>:<meter>:<bucketTS>
func bucketKey(scope string, r Rule, bucketTS time.Time) string {
	var buf [keyBufSize]byte
	b := append(buf[:0], "limit:{"...)
	b = append(b, scope...)
	b = append(b, "}:"...)
	b = append(b, r.Key...)
	b = append(b, ':')
	b = append(b, r.Meter...)
	b = append(b, ':')
	b = bucketTS.UTC().AppendFormat(b, time.RFC3339)
	return string(b)
}

// concurrencyKey returns the sorted-set key holding a concurrency rule's leases. The lease segment keeps it apart from the string counter older releases write under the bare name, so a rolling deploy never meets WRONGTYPE.
// format: limit:{<scope>}:lease:<rule.Key>:<meter>
func concurrencyKey(scope string, r Rule) string {
	return "limit:{" + scope + "}:lease:" + r.Key + ":" + r.Meter
}

// fixedWindowKey returns the state key for a fixed-window bucket.
// format: limit:{<scope>}:fw:<rule.Key>:<meter>:<bucketStartMs>
func fixedWindowKey(scope string, r Rule, bucketStartMs int64) string {
	var buf [keyBufSize]byte
	b := append(buf[:0], "limit:{"...)
	b = append(b, scope...)
	b = append(b, "}:fw:"...)
	b = append(b, r.Key...)
	b = append(b, ':')
	b = append(b, r.Meter...)
	b = append(b, ':')
	b = strconv.AppendInt(b, bucketStartMs, 10)
	return string(b)
}

// fixedWindowStartMs returns the start, in Unix ms, of the fixed window of length w containing now.
func fixedWindowStartMs(now time.Time, w time.Duration) int64 {
	return (now.UnixMilli() / w.Milliseconds()) * w.Milliseconds()
}

// windowCountKey returns the counter a reserve at now adds one to for r, and false when r keeps no such counter: only fixed- and sliding-window request meters do.
func windowCountKey(scope string, r Rule, now time.Time) (string, bool) {
	if r.Meter == MeterRevoked || r.Meter == MeterSpend || r.Meter == "concurrency" || isTokenMeter(r.Meter) {
		return "", false
	}
	switch r.Strategy {
	case StrategyFixedWindow:
		return fixedWindowKey(scope, r, fixedWindowStartMs(now, r.Window)), true
	case StrategySlidingWindow:
		cur, _ := windowBuckets(now, r.Window)
		return bucketKey(scope, r, cur), true
	}
	return "", false
}

// tbStateKey returns the state hash key for a token-bucket rule.
// format: limit:{<scope>}:tb:<rule.Key>:<meter>
func tbStateKey(scope string, r Rule) string {
	return "limit:{" + scope + "}:tb:" + r.Key + ":" + r.Meter
}

// lbStateKey returns the state hash key for a leaky-bucket rule.
// format: limit:{<scope>}:lb:<rule.Key>:<meter>
func lbStateKey(scope string, r Rule) string {
	return "limit:{" + scope + "}:lb:" + r.Key + ":" + r.Meter
}

// swStateKey returns the state hash key for a session-window rule.
// format: limit:{<scope>}:sw:<rule.Key>:<meter>
func swStateKey(scope string, r Rule) string {
	return "limit:{" + scope + "}:sw:" + r.Key + ":" + r.Meter
}

// spendKey returns the counter of a MeterSpend rule for the period starting at periodStart; a new period reads a new key, so the count starts from zero without a reset.
// format: limit:{<scope>}:spend:<rule.Key>:<period start, YYYY-MM-DD>
func spendKey(scope string, r Rule, periodStart time.Time) string {
	var buf [keyBufSize]byte
	b := append(buf[:0], "limit:{"...)
	b = append(b, scope...)
	b = append(b, "}:spend:"...)
	b = append(b, r.Key...)
	b = append(b, ':')
	b = periodStart.UTC().AppendFormat(b, time.DateOnly)
	return string(b)
}

// denyKey returns the existence key for a MeterRevoked rule. It carries no
// meter or strategy segment: the caller writes it directly (on revocation)
// and only its presence is read.
// format: limit:{<scope>}:<rule.Key>
func denyKey(scope string, r Rule) string {
	return "limit:{" + scope + "}:" + r.Key
}

// commitGuardKey returns the idempotency guard key for a reservation.
// format: limit:{<scope>}:committed:<reservationID>
func commitGuardKey(scope, reservationID string) string {
	return "limit:{" + scope + "}:committed:" + reservationID
}
