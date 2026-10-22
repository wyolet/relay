package keypool

import (
	"context"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// NewBatch returns a kv batch on the selector's store, so a caller can send PreparePick's reads in the same round trip as its own commands.
func (s *Selector) NewBatch() *kv.Batch { return kv.NewBatch(s.state) }

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
	var healthy []Candidate
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

// eligible reports whether c may take traffic. An open key past its OpenUntil is promoted to half-open (written back) and is eligible.
func (s *Selector) eligible(ctx context.Context, c Candidate, now time.Time) (Candidate, bool) {
	k, rec := c.Key, c.Record
	switch rec.State {
	case CircuitOpen:
		if rec.Indefinite { // written before auth cooldowns were bounded
			rec.Indefinite = false
			rec.OpenUntil = rec.LastTransition.Add(authCooldowns[len(authCooldowns)-1])
		}
		if now.Before(rec.OpenUntil) {
			return Candidate{}, false
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
		return Candidate{Key: k, Record: rec, Stored: true}, true
	case CircuitHalfOpen, CircuitClosed:
		return c, true
	default:
		return Candidate{}, false
	}
}
