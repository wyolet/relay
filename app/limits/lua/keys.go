// A counter's store keys come from its Group and ID alone: the group is the hash tag every key of one script shares, the ID the counter's identity inside it. pkg/ratelimit renders the full keys, every one with a TTL:
//
//	limit:{<group>}:tb:<id>:<meter>                     token bucket (lb:, sw: for leaky bucket and session window); TTL 2 × window
//	limit:{<group>}:fw:<id>:<meter>:<window start ms>   fixed window; TTL 2 × window
//	limit:{<group>}:<id>:<meter>:<bucket start>         sliding window and token meters; TTL 2 × window
//	limit:{<group>}:lease:<id>:concurrency              concurrency leases; TTL window + 1 s
//	limit:{<group>}:spend:<id>:<period start date>      budget spend in USD nanos; TTL to the period end + 1 h
//	limit:{<group>}:committed:<reservation id>          commit idempotency guard; TTL 1 min
//
// The group is part of every key, so a counter whose group changes (a project moved to another team) starts a fresh count.

package lua

import "github.com/wyolet/relay/app/limits"

// scope is the hash tag of c's keys.
func scope(c *limits.Counter) string { return c.Group }

// ruleKey is c's key segment inside its scope.
func ruleKey(c *limits.Counter) string { return c.ID }
