package keypool

import (
	"context"
	"errors"
	"strconv"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/reqid"
)

// KeySelection is the algorithm a Selector uses to pick from healthy
// candidates. Persisted as a string in Policy.Spec.KeySelection.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order — the
	// first healthy key in the candidate set wins. Default.
	KeySelectionPrioritized KeySelection = "prioritized"
	// KeySelectionRoundRobin rotates evenly across healthy keys via a
	// per-scope counter.
	KeySelectionRoundRobin KeySelection = "round-robin"
	// KeySelectionLeastRecentlyUsed prefers the key whose last successful
	// use was furthest in the past.
	KeySelectionLeastRecentlyUsed KeySelection = "least-recently-used"
)

var ErrNoHealthyKeys = errors.New("keypool: no healthy keys in pool")

// candidate holds a healthy HostKey alongside its circuit record.
type candidate struct {
	key     *hostkey.HostKey
	rec     CircuitRecord
	promote bool
}

// Pick returns a healthy HostKey from the candidate set. scope is the
// kv key tag (typically the owning Policy's Meta.Name) used for the
// round-robin counter and LRU timestamps. algo is the selection
// strategy; empty falls back to KeySelectionPrioritized.
//
// exclude is an optional list of keys to skip even if healthy (e.g. for
// retry-with-exclusion in future callers). Pass nil to skip the check.
//
// Open keys past their OpenUntil are auto-transitioned to HalfOpen and become
// eligible. Concurrent Picks may both pick the same half-open key; the
// caller's RecordSuccess/RecordFailure resolves the outcome (acceptable).
func (s *Selector) Pick(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude ...[]*hostkey.HostKey) (*hostkey.HostKey, error) {
	now := s.clock()

	// Build exclude set for O(1) lookup.
	var excludeSet map[string]struct{}
	if len(exclude) > 0 && len(exclude[0]) > 0 {
		excludeSet = make(map[string]struct{}, len(exclude[0]))
		for _, k := range exclude[0] {
			excludeSet[k.KeyHash] = struct{}{}
		}
	}

	var healthy []candidate
	for _, k := range keys {
		if excludeSet != nil {
			if _, skip := excludeSet[k.KeyHash]; skip {
				continue
			}
		}
		// The synthetic no-auth key has no credential to fail over to and
		// nothing to heal — a breaker would only strand the host's sole
		// candidate. Always treat it as healthy.
		if isAnonymous(k.KeyHash) {
			healthy = append(healthy, candidate{key: k, rec: CircuitRecord{State: CircuitClosed}})
			continue
		}
		rec := s.readRecord(ctx, k.KeyHash)

		switch rec.State {
		case CircuitOpen:
			if rec.Indefinite {
				continue
			}
			if now.Before(rec.OpenUntil) {
				continue
			}
			prior := rec.State
			rec.State = CircuitHalfOpen
			rec.LastTransition = now
			s.writeRecord(ctx, k.KeyHash, rec)
			s.log.Debug("keypool transition",
				"request_id", reqid.From(ctx),
				"key_hash", k.KeyHash,
				"from_state", stateName(prior),
				"to_state", stateName(rec.State),
				"reason", "open_expired",
				"backoff_step", rec.BackoffStep,
				"open_for_seconds", 0,
			)
			healthy = append(healthy, candidate{key: k, rec: rec, promote: true})
		case CircuitHalfOpen:
			healthy = append(healthy, candidate{key: k, rec: rec})
		case CircuitClosed:
			healthy = append(healthy, candidate{key: k, rec: rec})
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoHealthyKeys
	}

	strategy := algo
	if strategy == "" {
		strategy = KeySelectionPrioritized
	}

	switch strategy {
	case KeySelectionRoundRobin:
		return s.pickRoundRobin(ctx, scope, healthy)

	case KeySelectionLeastRecentlyUsed:
		return s.pickLRU(ctx, scope, healthy)

	default: // "prioritized" or empty — first healthy in declaration order.
		return healthy[0].key, nil
	}
}

// PickWithExclude is a convenience wrapper around Pick that accepts an explicit
// exclude list. Callers that always have an exclude slice can use this to avoid
// the variadic syntax.
func (s *Selector) PickWithExclude(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude []*hostkey.HostKey) (*hostkey.HostKey, error) {
	return s.Pick(ctx, scope, algo, keys, exclude)
}

// pickRoundRobin selects a candidate using a modular counter stored in Redis.
// Incr is atomic on every backend, so the index derives directly from the
// counter value — no distributed lock, no extra round-trip on the hot path
// (audit 2026-07-04 P1 tracker #9: the old WithLock wrapper collapsed
// rotation to healthy[0] under contention on the non-blocking Redis lock).
func (s *Selector) pickRoundRobin(ctx context.Context, scope string, healthy []candidate) (*hostkey.HostKey, error) {
	idx, err := s.state.Incr(ctx, roundRobinKey(scope), 1)
	if err != nil {
		idx = 1
	} else {
		// Refresh 30-day TTL on every increment. Redis reclaims counters for
		// deleted pools; a counter that expires merely restarts the rotation
		// at healthy[0], which is harmless.
		_ = s.state.Expire(ctx, roundRobinKey(scope), ttlRoundRobin)
	}
	i := (idx - 1) % int64(len(healthy))
	if i < 0 { // negative counter (int64 wraparound or external edit)
		i += int64(len(healthy))
	}
	return healthy[i].key, nil
}

// pickLRU selects the healthy candidate with the oldest last-use timestamp
// (or never-used) and updates its timestamp.
func (s *Selector) pickLRU(ctx context.Context, scope string, healthy []candidate) (*hostkey.HostKey, error) {
	var (
		chosen    *hostkey.HostKey
		chosenTS  int64 = -1 // -1 = not yet set
		neverUsed *hostkey.HostKey
	)

	for _, c := range healthy {
		k := lruKey(scope, c.key.KeyHash)
		raw, err := s.state.Get(ctx, k)
		if err != nil || len(raw) == 0 {
			// Never used — immediately preferred.
			if neverUsed == nil {
				neverUsed = c.key
			}
			continue
		}
		ts, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			if neverUsed == nil {
				neverUsed = c.key
			}
			continue
		}
		if chosen == nil || ts < chosenTS {
			chosen = c.key
			chosenTS = ts
		}
	}

	if neverUsed != nil {
		chosen = neverUsed
	}
	if chosen == nil {
		// Fallback: should not happen given len(healthy) > 0.
		chosen = healthy[0].key
	}

	// Stamp last-use timestamp.
	now := s.clock().UnixMilli()
	k := lruKey(scope, chosen.KeyHash)
	_ = s.state.Set(ctx, k, []byte(strconv.FormatInt(now, 10)), ttlLRU)

	return chosen, nil
}
