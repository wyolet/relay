package keypool

import (
	"context"
	"errors"
	"strconv"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
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
//
// The reads go out in one kv batch; see PreparePick to share it.
func (s *Selector) Pick(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude ...[]*hostkey.HostKey) (*hostkey.HostKey, error) {
	var excluded []*hostkey.HostKey
	if len(exclude) > 0 {
		excluded = exclude[0]
	}
	b := s.NewBatch()
	p := s.PreparePick(b, scope, algo, keys, excluded)
	b.Exec(ctx)
	return p.Finish(ctx)
}

// PickWithExclude is a convenience wrapper around Pick that accepts an explicit
// exclude list. Callers that always have an exclude slice can use this to avoid
// the variadic syntax.
func (s *Selector) PickWithExclude(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude []*hostkey.HostKey) (*hostkey.HostKey, error) {
	return s.Pick(ctx, scope, algo, keys, exclude)
}

// pickRoundRobin selects a candidate by the rotation counter's INCR result.
// Incr is atomic on every backend, so the index derives directly from the
// counter value — no distributed lock, which under contention on the
// non-blocking Redis lock would collapse rotation to healthy[0]. A failed
// INCR falls back to healthy[0].
func pickRoundRobin(rotation *kv.BatchResult, healthy []candidate) *hostkey.HostKey {
	idx := int64(1)
	if rotation != nil && rotation.Err == nil {
		idx = rotation.Int
	}
	i := (idx - 1) % int64(len(healthy))
	if i < 0 { // negative counter (int64 wraparound or external edit)
		i += int64(len(healthy))
	}
	return healthy[i].key
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
