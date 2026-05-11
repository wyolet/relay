package ratelimit

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/internal/catalog"
)

// resolvedMeter returns the effective meter string for a ResolvedRule,
// preferring the typed Rule.Meter over the legacy Meter field.
func resolvedMeter(r catalog.ResolvedRule) string {
	if r.Rule.Meter != "" {
		return r.Rule.Meter
	}
	return string(r.Meter)
}

// resolvedRLName returns the effective RateLimit name for a ResolvedRule,
// preferring RateLimitName over the legacy RateLimit.Metadata.Name.
func resolvedRLName(r catalog.ResolvedRule) string {
	if r.RateLimitName != "" {
		return r.RateLimitName
	}
	if r.RateLimit != nil {
		return r.RateLimit.Metadata.Name
	}
	return ""
}

// bucketKey returns the state key for a sliding-window bucket.
// format: limit:{policy:<poolName>}:<parentKind>:<parentName>:<rlName>:<meter>:<bucketTS>
// The {policy:<poolName>} hash tag pins all keys for a single request to the
// same Redis Cluster slot regardless of parentKind.
func bucketKey(poolName string, r catalog.ResolvedRule, bucketTS time.Time) string {
	return fmt.Sprintf("limit:{policy:%s}:%s:%s:%s:%s:%s",
		poolName,
		r.ParentKind, r.ParentName,
		resolvedRLName(r),
		resolvedMeter(r),
		bucketTS.UTC().Format(time.RFC3339),
	)
}

// concurrencyKey returns the state key for a concurrency counter.
// format: limit:{policy:<poolName>}:<parentKind>:<parentName>:<rlName>:<meter>
func concurrencyKey(poolName string, r catalog.ResolvedRule) string {
	return fmt.Sprintf("limit:{policy:%s}:%s:%s:%s:%s",
		poolName,
		r.ParentKind, r.ParentName,
		resolvedRLName(r),
		resolvedMeter(r),
	)
}

// commitGuardKey returns the idempotency guard key for a reservation.
// format: limit:{policy:<poolName>}:committed:<reservationID>
// The policy hash tag must match the concurrency/token keys touched in the
// same Commit Lua call to avoid a CROSSSLOT error in Cluster mode.
func commitGuardKey(poolName, reservationID string) string {
	return fmt.Sprintf("limit:{policy:%s}:committed:%s", poolName, reservationID)
}
