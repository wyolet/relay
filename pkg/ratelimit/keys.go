package ratelimit

import (
	"fmt"
	"time"
)

// bucketKey returns the state key for a sliding-window bucket.
// format: limit:{<scope>}:<rule.Key>:<meter>:<bucketTS>
func bucketKey(scope string, r Rule, bucketTS time.Time) string {
	return fmt.Sprintf("limit:{%s}:%s:%s:%s",
		scope, r.Key, r.Meter,
		bucketTS.UTC().Format(time.RFC3339),
	)
}

// concurrencyKey returns the state key for a concurrency counter.
// format: limit:{<scope>}:<rule.Key>:<meter>
func concurrencyKey(scope string, r Rule) string {
	return fmt.Sprintf("limit:{%s}:%s:%s", scope, r.Key, r.Meter)
}

// fixedWindowKey returns the state key for a fixed-window bucket.
// format: limit:{<scope>}:fw:<rule.Key>:<meter>:<bucketStartMs>
func fixedWindowKey(scope string, r Rule, bucketStartMs int64) string {
	return fmt.Sprintf("limit:{%s}:fw:%s:%s:%d", scope, r.Key, r.Meter, bucketStartMs)
}

// tbStateKey returns the state hash key for a token-bucket rule.
// format: limit:{<scope>}:tb:<rule.Key>:<meter>
func tbStateKey(scope string, r Rule) string {
	return fmt.Sprintf("limit:{%s}:tb:%s:%s", scope, r.Key, r.Meter)
}

// lbStateKey returns the state hash key for a leaky-bucket rule.
// format: limit:{<scope>}:lb:<rule.Key>:<meter>
func lbStateKey(scope string, r Rule) string {
	return fmt.Sprintf("limit:{%s}:lb:%s:%s", scope, r.Key, r.Meter)
}

// swStateKey returns the state hash key for a session-window rule.
// format: limit:{<scope>}:sw:<rule.Key>:<meter>
func swStateKey(scope string, r Rule) string {
	return fmt.Sprintf("limit:{%s}:sw:%s:%s", scope, r.Key, r.Meter)
}

// commitGuardKey returns the idempotency guard key for a reservation.
// format: limit:{<scope>}:committed:<reservationID>
func commitGuardKey(scope, reservationID string) string {
	return fmt.Sprintf("limit:{%s}:committed:%s", scope, reservationID)
}
