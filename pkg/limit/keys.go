package limit

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/internal/catalog"
)

// bucketKey returns the state key for a sliding-window bucket.
// format: limit:{pool:<poolName>}:<parentKind>:<parentName>:<rlName>:<meter>:<bucketTS>
// The {pool:<poolName>} hash tag pins all keys for a single request to the
// same Redis Cluster slot regardless of parentKind.
func bucketKey(poolName string, r catalog.ResolvedRule, bucketTS time.Time) string {
	return fmt.Sprintf("limit:{pool:%s}:%s:%s:%s:%s:%s",
		poolName,
		r.ParentKind, r.ParentName,
		r.RateLimit.Metadata.Name,
		r.Meter,
		bucketTS.UTC().Format(time.RFC3339),
	)
}

// concurrencyKey returns the state key for a concurrency counter.
// format: limit:{pool:<poolName>}:<parentKind>:<parentName>:<rlName>:<meter>
func concurrencyKey(poolName string, r catalog.ResolvedRule) string {
	return fmt.Sprintf("limit:{pool:%s}:%s:%s:%s:%s",
		poolName,
		r.ParentKind, r.ParentName,
		r.RateLimit.Metadata.Name,
		r.Meter,
	)
}

// commitGuardKey returns the idempotency guard key for a reservation.
// format: limit:{pool:<poolName>}:committed:<reservationID>
// The pool hash tag must match the concurrency/token keys touched in the
// same Commit Lua call to avoid a CROSSSLOT error in Cluster mode.
func commitGuardKey(poolName, reservationID string) string {
	return fmt.Sprintf("limit:{pool:%s}:committed:%s", poolName, reservationID)
}
