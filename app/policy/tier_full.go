package policy

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// ErrKeyTiersFull: every candidate key is over its tier limit for the requested model. It wraps a *pkgratelimit.KeyQuotaExhausted whose RetryAfter is the earliest time one of them has room.
var ErrKeyTiersFull = errors.New("policy: every host key is over its tier rate limit")

// tierFullCap bounds the remembered (key, model) pairs. Past it a refusal is not remembered and the key is tried again, which costs a refused reserve, not a request.
const tierFullCap = 4096

// concurrencyFullFor caps how long a concurrency refusal is remembered: its retry-after is the oldest lease's expiry, while a slot usually frees as soon as a request ends.
const concurrencyFullFor = 250 * time.Millisecond

type tierFullKey struct{ keyID, modelID string }

type tierFullEntry struct {
	until time.Time
	rule  pkgratelimit.Rule
}

// tierFull remembers, per process, which keys refused a tier reserve for a model and until when. Skipping them before the pick saves the refused reserve and the re-pick a request would otherwise pay on every attempt until the window has room. Per process on purpose: each pod learns from its own refusal, and the shared tier counters stay the authority.
type tierFull struct {
	mu      sync.RWMutex
	entries map[tierFullKey]tierFullEntry
	// size mirrors len(entries) so the common case, nothing remembered, takes no lock.
	size atomic.Int64
}

func (t *tierFull) record(keyID, modelID string, exceeded *pkgratelimit.KeyQuotaExhausted, now time.Time) {
	wait := exceeded.RetryAfter
	if exceeded.Rule.Meter == string(ratelimit.MeterConcurrency) {
		wait = min(wait, concurrencyFullFor)
	}
	if wait <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[tierFullKey]tierFullEntry)
	}
	if len(t.entries) >= tierFullCap {
		for k, e := range t.entries {
			if !now.Before(e.until) {
				delete(t.entries, k)
			}
		}
		if len(t.entries) >= tierFullCap {
			return
		}
	}
	t.entries[tierFullKey{keyID, modelID}] = tierFullEntry{until: now.Add(wait), rule: exceeded.Rule}
	t.size.Store(int64(len(t.entries)))
}

// exclude returns base plus the keys still remembered as full for modelID, and the refusal for the one with room soonest (nil when none is full). base is returned as is when nothing is remembered.
func (t *tierFull) exclude(keys, base []*hostkey.HostKey, modelID string, now time.Time) ([]*hostkey.HostKey, *pkgratelimit.KeyQuotaExhausted) {
	if t.size.Load() == 0 {
		return base, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var (
		out      = base
		earliest *tierFullEntry
	)
	for _, k := range keys {
		e, ok := t.entries[tierFullKey{k.Meta.ID, modelID}]
		if !ok || !now.Before(e.until) {
			continue
		}
		if len(out) == len(base) {
			out = append(make([]*hostkey.HostKey, 0, len(base)+len(keys)), base...)
		}
		out = append(out, k)
		if earliest == nil || e.until.Before(earliest.until) {
			earliest = &e
		}
	}
	if earliest == nil {
		return base, nil
	}
	return out, &pkgratelimit.KeyQuotaExhausted{Rule: earliest.rule, RetryAfter: earliest.until.Sub(now)}
}

// keyTiersFullError is the error for a request whose every candidate key is remembered as full.
func keyTiersFullError(exceeded *pkgratelimit.KeyQuotaExhausted) error {
	return fmt.Errorf("%w: %w", ErrKeyTiersFull, exceeded)
}
