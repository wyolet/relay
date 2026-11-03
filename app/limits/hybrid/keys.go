// Local counts are written to the keys the Lua limiter's scripts read (app/limits/lua/keys.go), rendered by pkg/ratelimit from the counter's group and ID, with the same TTLs: a key that has none gets it on the first write (EXPIRE NX), so a sync never extends a live key's deadline.

package hybrid

import (
	"time"

	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// counterKey is the store key of the counter r counts in scope for the bucket starting at start, and the TTL a write at now gives it.
func counterKey(scope string, r pkgratelimit.Rule, start int64, now time.Time) (string, time.Duration, error) {
	return pkgratelimit.CounterKey(scope, r, time.UnixMilli(start), now)
}
