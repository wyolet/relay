package limit

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
)

// bucketKey returns the state key for a sliding-window bucket.
// format: limit:<parentKind>:<parentName>:<rlName>:requests:<bucketTS>
func bucketKey(r configstore.ResolvedRule, bucketTS time.Time) string {
	return fmt.Sprintf("limit:%s:%s:%s:%s:%s",
		r.ParentKind, r.ParentName,
		r.RateLimit.Metadata.Name,
		r.Meter,
		bucketTS.UTC().Format(time.RFC3339),
	)
}

// concurrencyKey returns the state key for a concurrency counter.
// format: limit:<parentKind>:<parentName>:<rlName>:concurrency
func concurrencyKey(r configstore.ResolvedRule) string {
	return fmt.Sprintf("limit:%s:%s:%s:%s",
		r.ParentKind, r.ParentName,
		r.RateLimit.Metadata.Name,
		r.Meter,
	)
}

// commitGuardKey returns the idempotency guard key for a reservation.
func commitGuardKey(reservationID string) string {
	return "limit:committed:" + reservationID
}
