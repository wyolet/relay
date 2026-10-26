package keypool

import (
	"context"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// NewBatch returns a kv batch on the selector's store with room for size commands, so a caller can send PreparePick's reads in the same round trip as its own commands.
func (s *Selector) NewBatch(size int) *kv.Batch { return kv.NewBatch(s.state, size) }

// PickOps is the most commands PreparePick queues for n keys under algo.
func PickOps(algo KeySelection, n int) int {
	switch algo {
	case KeySelectionRoundRobin:
		return n + 2
	case KeySelectionLeastRecentlyUsed:
		return n
	default:
		return min(n, 1)
	}
}

// PreparedPick is a Pick whose reads are queued on a kv batch; Finish chooses the key once the batch has run.
type PreparedPick struct {
	s     *Selector
	scope string
	algo  KeySelection
	keys  []*hostkey.HostKey
	skip  map[string]struct{}
	// reads has one entry per key; nil where the batch read nothing (an excluded or anonymous key, or a prioritized key behind the first).
	reads       []*kv.BatchResult
	rotation    *kv.BatchResult
	rotationTTL *kv.BatchResult
}

// PreparePick queues Pick's reads for keys on b: each candidate's breaker record (prioritized: the first candidate's only) and, for round-robin, the rotation counter. The caller runs b, then calls Finish.
func (s *Selector) PreparePick(b *kv.Batch, scope string, algo KeySelection, keys, exclude []*hostkey.HostKey) *PreparedPick {
	if algo != KeySelectionRoundRobin && algo != KeySelectionLeastRecentlyUsed {
		algo = KeySelectionPrioritized
	}
	p := &PreparedPick{s: s, scope: scope, algo: algo, keys: keys, reads: make([]*kv.BatchResult, len(keys))}
	if len(exclude) > 0 {
		p.skip = make(map[string]struct{}, len(exclude))
		for _, k := range exclude {
			p.skip[k.KeyHash] = struct{}{}
		}
	}
	for i, k := range keys {
		if _, skip := p.skip[k.KeyHash]; skip {
			continue
		}
		if !isAnonymous(k.KeyHash) {
			p.reads[i] = b.Get(circuitKey(k.KeyHash))
		}
		// Prioritized takes the first fit key in order; the ones behind it are read by Finish only while it is open.
		if algo == KeySelectionPrioritized {
			break
		}
	}
	if algo == KeySelectionRoundRobin {
		p.rotation = b.Incr(roundRobinKey(scope), 1)
		// NX leaves a live counter's deadline alone and gives one that lost its TTL a new one, without a round trip of its own.
		p.rotationTTL = b.ExpireNX(roundRobinKey(scope), ttlRoundRobin)
	}
	return p
}

// Finish chooses the key from the batch's results, reading on its own only the prioritized candidates the batch skipped.
func (p *PreparedPick) Finish(ctx context.Context) (Candidate, error) {
	s := p.s
	now := s.clock()
	if p.rotationTTL != nil && p.rotationTTL.Err != nil {
		metrics.KVError("key_rotation", metrics.KVDropped)
	}
	n := len(p.keys)
	if p.algo == KeySelectionPrioritized {
		n = min(n, 1)
	}
	healthy := make([]Candidate, 0, n)
	for i, k := range p.keys {
		if _, skip := p.skip[k.KeyHash]; skip {
			continue
		}
		// The synthetic no-auth key has no credential to fail over to and nothing to heal — a breaker would only strand the host's sole candidate. Always treat it as healthy.
		if isAnonymous(k.KeyHash) {
			healthy = append(healthy, Candidate{Key: k, Record: CircuitRecord{State: CircuitClosed}})
		} else {
			var c Candidate
			if r := p.reads[i]; r != nil {
				c.Record, c.Stored = recordFrom(r.Value, r.Err)
			} else {
				c.Record, c.Stored = recordFrom(s.state.Get(ctx, circuitKey(k.KeyHash)))
			}
			c.Key = k
			c, ok := s.eligible(ctx, c, now)
			if !ok {
				continue
			}
			healthy = append(healthy, c)
		}
		if p.algo == KeySelectionPrioritized {
			break
		}
	}
	if len(healthy) == 0 {
		return Candidate{}, ErrNoHealthyKeys
	}
	switch p.algo {
	case KeySelectionRoundRobin:
		return pickRoundRobin(p.rotation, healthy), nil
	case KeySelectionLeastRecentlyUsed:
		return s.pickLRU(ctx, p.scope, healthy), nil
	default:
		return healthy[0], nil
	}
}

// eligible reports whether c may take traffic. A key whose cooldown has ended (open past OpenUntil, or half-open with a lapsed probe lease) is eligible only to the pick that claims its probe; the rest skip it until the probe's outcome lands or the lease lapses.
func (s *Selector) eligible(ctx context.Context, c Candidate, now time.Time) (Candidate, bool) {
	switch c.Record.State {
	case CircuitClosed:
		return c, true
	case CircuitOpen:
		if now.Before(c.Record.OpenUntil) {
			return Candidate{}, false
		}
	case CircuitHalfOpen:
		if now.Before(c.Record.ProbeUntil) {
			return Candidate{}, false
		}
	default:
		return Candidate{}, false
	}
	rec, ok := s.claimProbe(ctx, c.Key.KeyHash, c.Record, now)
	if !ok {
		return Candidate{}, false
	}
	return Candidate{Key: c.Key, Record: rec, Stored: true}, true
}

// claimProbe takes the key's probe lease. A failed script admits the pick on the record it read, as a failed read would.
func (s *Selector) claimProbe(ctx context.Context, keyHash string, read CircuitRecord, now time.Time) (CircuitRecord, bool) {
	raw, err := s.runRecordScript(ctx, claimCall(keyHash, now), func(rec CircuitRecord, found bool) (CircuitRecord, bool) {
		if !found {
			return rec, false
		}
		return claimProbe(rec, now, probeLease)
	})
	var out recordOutcome
	if err == nil {
		out, err = decodeOutcome(raw)
	}
	if err != nil {
		metrics.KVError("breaker_write", metrics.KVFallback)
		s.log.Error("keypool: claim probe script failed", "key_hash", keyHash, "err", err)
		read.State = CircuitHalfOpen
		return read, true
	}
	if !out.changed {
		return CircuitRecord{}, false
	}
	s.log.Debug("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(out.prior),
		"to_state", stateName(out.rec.State),
		"reason", "probe",
		"backoff_step", out.rec.BackoffStep,
		"open_for_seconds", 0,
	)
	return out.rec, true
}
