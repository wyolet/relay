// Package keypool implements per-key circuit-breaker state and configurable
// Pool selection over healthy keys. State is persisted in pkg/state under
// "secret_health:<keyHash>" (circuit records), "pool_rr:<poolName>"
// (round-robin counters), and "pool_lru:<poolName>:<keyHash>"
// (LRU timestamps).
//
// Supported selection strategies (KeySelection):
//   - "prioritized" (default) — always pick the first healthy key in declaration order.
//   - "round-robin" — distribute traffic evenly using a counter.
//   - "least-recently-used" — pick the key with the oldest last-used timestamp.
//
// The KeySelection enum lives in this package (not policy) so policy can
// import keypool without creating a cycle when its Service composes
// Selector + Limiter at runtime.
package keypool

import (
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
)

const (
	// ttlFlat is the TTL applied to all non-indefinite circuit-breaker records
	// so they persist past OpenUntil for debugging.
	ttlFlat = 24 * time.Hour

	// ttlRoundRobin is the TTL on pool_rr counters. The counter is a modular
	// index so staleness is harmless, but a long TTL lets Redis reclaim keys
	// from deleted pools instead of accumulating indefinitely.
	ttlRoundRobin = 30 * 24 * time.Hour

	// ttlLRU is the TTL on pool_lru timestamps. Long enough to survive between
	// pool deployments; staleness is harmless (treated as never-used).
	ttlLRU = 30 * 24 * time.Hour
)

// Selector picks HostKeys from Pools and tracks per-key circuit-breaker state.
type Selector struct {
	state  kv.Store
	runner kv.Scripter // set when state supports server-side scripts
	log    *slog.Logger
	clock  func() time.Time
	rng    *rand.Rand
}

// New constructs a Selector. clock and rng may be nil.
// When rng is nil, a new rand seeded from time.Now().UnixNano() is used.
func New(s kv.Store, log *slog.Logger, clock func() time.Time, rng *rand.Rand) *Selector {
	if clock == nil {
		clock = time.Now
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	sel := &Selector{state: s, log: log, clock: clock, rng: rng}
	if sr, ok := s.(kv.Scripter); ok {
		sel.runner = sr
	}
	if ms, ok := s.(*kv.Mem); ok {
		RegisterScripts(ms)
	}
	return sel
}

// isAnonymous reports whether keyHash belongs to the synthetic no-auth key
// routing injects for a NoAuth host. Such keys are exempt from the circuit
// breaker: there is no credential to fail over to and nothing to heal.
func isAnonymous(keyHash string) bool {
	return strings.HasPrefix(keyHash, hostkey.AnonIDPrefix)
}
