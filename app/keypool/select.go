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

// Candidate is a key fit for traffic and the breaker record it was judged on.
type Candidate struct {
	Key    *hostkey.HostKey
	Record CircuitRecord
	// Stored is false when the store held no record for the key (or the read
	// failed) and Record is the closed default.
	Stored bool
}

// SuccessNeedsWrite reports whether a success on c changes its record: a
// half-open probe or a record carrying a backoff step. A clean closed record,
// or none, already reads as healthy, and writing over it could erase a
// failure another pod recorded since the pick.
func (c Candidate) SuccessNeedsWrite() bool {
	return c.Stored && (c.Record.State != CircuitClosed || c.Record.BackoffStep != 0)
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
	c, err := s.PickCandidate(ctx, scope, algo, keys, excluded)
	return c.Key, err
}

// PickCandidate is Pick returning the record the key was picked on, so the
// caller can tell whether its success needs writing.
func (s *Selector) PickCandidate(ctx context.Context, scope string, algo KeySelection, keys, exclude []*hostkey.HostKey) (Candidate, error) {
	b := s.NewBatch()
	p := s.PreparePick(b, scope, algo, keys, exclude)
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
func pickRoundRobin(rotation *kv.BatchResult, healthy []Candidate) Candidate {
	idx := int64(1)
	if rotation != nil && rotation.Err == nil {
		idx = rotation.Int
	}
	i := (idx - 1) % int64(len(healthy))
	if i < 0 { // negative counter (int64 wraparound or external edit)
		i += int64(len(healthy))
	}
	return healthy[i]
}

// pickLRU selects the healthy candidate with the oldest last-use timestamp
// (or never-used) and updates its timestamp.
func (s *Selector) pickLRU(ctx context.Context, scope string, healthy []Candidate) Candidate {
	var (
		chosen    = -1
		chosenTS  int64
		neverUsed = -1
	)

	for i, c := range healthy {
		k := lruKey(scope, c.Key.KeyHash)
		raw, err := s.state.Get(ctx, k)
		if err != nil || len(raw) == 0 {
			// Never used — immediately preferred.
			if neverUsed < 0 {
				neverUsed = i
			}
			continue
		}
		ts, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			if neverUsed < 0 {
				neverUsed = i
			}
			continue
		}
		if chosen < 0 || ts < chosenTS {
			chosen = i
			chosenTS = ts
		}
	}

	if neverUsed >= 0 {
		chosen = neverUsed
	}
	if chosen < 0 {
		// Fallback: should not happen given len(healthy) > 0.
		chosen = 0
	}

	// Stamp last-use timestamp.
	now := s.clock().UnixMilli()
	k := lruKey(scope, healthy[chosen].Key.KeyHash)
	_ = s.state.Set(ctx, k, []byte(strconv.FormatInt(now, 10)), ttlLRU)

	return healthy[chosen]
}
