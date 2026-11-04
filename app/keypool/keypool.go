// Package keypool implements per-key circuit-breaker state and configurable Pool selection over healthy keys. State is persisted in pkg/kv under "secret_health:{secret:<keyHash>}" (circuit records), "pool_rr:{pool:<scope>}" (round-robin counters), and "{pool_lru:<scope>}:<keyHash>" (LRU timestamps); the scope is the caller's policy id.
//
// Supported selection strategies (KeySelection):
//   - "prioritized" (default) — always pick the first healthy key in declaration order.
//   - "round-robin" — distribute traffic evenly using a counter.
//   - "least-recently-used" — pick the key with the oldest last-used timestamp.
//
// A Selector with a PickState picks from the process's memory instead: breaker records are copies reread once per refresh interval, and the round-robin counters and LRU stamps live there alone, so rotation is per process rather than fleet-wide. Its own transitions apply to the copy at once; another process's reach it within one interval, so it may send up to an interval of requests to a key another process has just seen fail. The probe claim stays a store script: it needs fleet-wide exclusion.
//
// Expected kv ops per request:
//   - Pick with a PickState: none while the records it reads are fresh (read or written within two intervals). A record not yet held, or stale, is read through: a GET queued on the pick's batch.
//   - Pick without one: one kv.Batch round trip with a GET of each candidate's breaker record and, for round-robin, the counter's INCR and EXPIRE NX. Prioritized reads only the first candidate in the batch; the ones behind it cost a GET each, and only while the keys ahead are open. Least-recently-used adds a GET per healthy candidate and a SET. PreparePick queues the same reads on a caller's batch (the pipeline sends them with the caller's hold).
//   - Either way, claiming the probe of a key whose cooldown ended: one script.
//   - PickState refresh, per interval and off the request path: one pipelined round trip with a GET per record picked since the last refresh.
//   - RecordSuccess: one script; PrepareRecordSuccess queues it on a caller's batch. A key's record exists only once it has failed: post-flight writes a success only for a half-open probe, a record with a backoff step, or a failure count (Candidate.SuccessNeedsWrite). RecordFailure: one script; PrepareRecordFailure queues it on a caller's batch. Claiming a half-open probe in Pick: one script.
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

	// DefaultFailureThreshold and DefaultFailureWindow apply when WithBreaker is not given positive values: a closed key opens on this many server or network errors within the window, with no success between them.
	DefaultFailureThreshold = 5
	DefaultFailureWindow    = 30 * time.Second

	// probeLease bounds how long one half-open probe keeps other picks off a key. A probe's outcome lands only when its response ends, so a long stream lets one more probe through per lease rather than holding the key out for the stream's length.
	probeLease = 10 * time.Second

	// recordTimeout bounds a failure write, which runs detached from the request that saw the failure.
	recordTimeout = 2 * time.Second
)

// Selector picks HostKeys from Pools and tracks per-key circuit-breaker state.
type Selector struct {
	state  kv.Store
	runner kv.Scripter // set when state supports server-side scripts
	log    *slog.Logger
	clock  func() time.Time
	rng    *rand.Rand

	failureThreshold int
	failureWindow    time.Duration

	// local, when set, serves picks from this pod's memory: breaker records refreshed per interval, rotation and LRU stamps kept here alone.
	local *PickState
}

// WithPickState makes s pick from ps instead of reading the store per pick, and write its own transitions through to ps. Give every selector of the process the same ps. Call before serving.
func (s *Selector) WithPickState(ps *PickState) *Selector {
	s.local = ps
	return s
}

// wrote keeps rec, the record a transition left in the store, in the PickState.
func (s *Selector) wrote(keyHash string, rec CircuitRecord) {
	if s.local != nil {
		s.local.remember(keyHash, rec, true, s.clock(), false)
	}
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
	sel := &Selector{state: s, log: log, clock: clock, rng: rng,
		failureThreshold: DefaultFailureThreshold, failureWindow: DefaultFailureWindow}
	if sr, ok := s.(kv.Scripter); ok {
		sel.runner = sr
	}
	if ms, ok := s.(*kv.Mem); ok {
		RegisterScripts(ms)
	}
	return sel
}

// WithBreaker sets how many server or network errors within window open a closed key. Values below 1 keep the defaults.
func (s *Selector) WithBreaker(threshold int, window time.Duration) *Selector {
	if threshold > 0 {
		s.failureThreshold = threshold
	}
	if window > 0 {
		s.failureWindow = window
	}
	return s
}

// isAnonymous reports whether keyHash belongs to the synthetic no-auth key
// routing injects for a NoAuth host. Such keys are exempt from the circuit
// breaker: there is no credential to fail over to and nothing to heal.
func isAnonymous(keyHash string) bool {
	return strings.HasPrefix(keyHash, hostkey.AnonIDPrefix)
}
