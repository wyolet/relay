package tokencount

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

const (
	// sessionTTL outlives a coding session's idle gaps but not the day: a conversation's prompt shape stops being evidence once it is over.
	sessionTTL = 24 * time.Hour

	// modelTTL keeps the coarser fallback across restarts and quiet weekends.
	modelTTL = 7 * 24 * time.Hour
)

const (
	// rewriteThreshold is the relative move a sample must make against the ratio this process last wrote before it is worth a kv write. A smaller move barely changes an estimate.
	rewriteThreshold = 0.05

	// rewriteInterval re-sends a ratio that has not moved, so an active key's TTL keeps being extended and a value another pod overwrote is corrected. Far inside sessionTTL, so a key in use never lapses.
	rewriteInterval = 5 * time.Minute

	// maxWrittenEntries bounds the per-process record of written ratios: session ids accrue without limit, and a forgotten key costs one extra write and nothing else.
	maxWrittenEntries = 10_000
)

// store is the narrow kv surface a Calibrator needs: one ratio per key, last write wins, every key expiring on its own.
type store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// writtenRatio is what this process last sent to one key, and when.
type writtenRatio struct {
	ratio float64
	at    time.Time
}

// Calibrator records and reads the observed tokens-per-byte ratio. Construct once at boot over the shared kv store; safe for concurrent use.
type Calibrator struct {
	state store
	now   func() time.Time

	mu      sync.Mutex
	written map[string]writtenRatio
}

// NewCalibrator constructs a Calibrator over s.
func NewCalibrator(s kv.Store) *Calibrator { return &Calibrator{state: s} }

// Observe records one true measurement: inputTokens is what the upstream reported for a request whose body was bodyBytes long. Both ids are optional — an empty or unusable one is skipped, so a request with no session still teaches the model ratio. Non-positive inputs are ignored rather than stored as a zero ratio. A key is written only when the ratio moved or its last write is due for a refresh (see claimWrite).
//
// Errors are dropped on purpose: a lost sample costs the next estimate some accuracy and nothing else, and this runs in post-flight where there is no caller to tell.
func (c *Calibrator) Observe(ctx context.Context, sessionID, modelID string, bodyBytes, inputTokens int) {
	if c == nil || c.state == nil || bodyBytes <= 0 || inputTokens <= 0 {
		return
	}
	r := float64(inputTokens) / float64(bodyBytes)
	if id := safeID(sessionID); id != "" {
		c.write(ctx, sessionKey(id), r, sessionTTL)
	}
	if id := safeID(modelID); id != "" {
		c.write(ctx, modelKey(id), r, modelTTL)
	}
}

// write sends r to key when claimWrite allows it.
func (c *Calibrator) write(ctx context.Context, key string, r float64, ttl time.Duration) {
	if !c.claimWrite(key, r) {
		ratioWrites.WithLabelValues(outcomeSkipped).Inc()
		return
	}
	if err := c.state.Set(ctx, key, formatRatio(r), ttl); err != nil {
		ratioWrites.WithLabelValues(outcomeFailed).Inc()
		metrics.KVError("token_ratio", metrics.KVDropped)
		return
	}
	ratioWrites.WithLabelValues(outcomeWritten).Inc()
}

// claimWrite reports whether r should be written to key, and if so records it as written. A sample is compared with the ratio last written rather than the last one seen, so a slow drift still crosses the threshold. The claim is taken before the write, so concurrent requests for one key send it once; a failed write keeps its claim, because a failing kv store is when a retry on every request would hurt most.
func (c *Calibrator) claimWrite(key string, r float64) bool {
	now := c.clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.written[key]
	if ok && now.Sub(last.at) < rewriteInterval && math.Abs(r-last.ratio) <= rewriteThreshold*last.ratio {
		return false
	}
	if c.written == nil {
		c.written = make(map[string]writtenRatio)
	}
	if !ok && len(c.written) >= maxWrittenEntries {
		c.evictLocked(now)
	}
	c.written[key] = writtenRatio{ratio: r, at: now}
	return true
}

// evictLocked makes room in a full record. Entries past rewriteInterval go first, as their next sample is written anyway; if that frees too little, arbitrary entries go until a quarter is free, so a record full of live keys is not rescanned on every insert. c.mu must be held.
func (c *Calibrator) evictLocked(now time.Time) {
	for k, w := range c.written {
		if now.Sub(w.at) >= rewriteInterval {
			delete(c.written, k)
		}
	}
	for k := range c.written {
		if len(c.written) <= maxWrittenEntries*3/4 {
			break
		}
		delete(c.written, k)
	}
}

func (c *Calibrator) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Ratio returns the tokens-per-byte ratio to estimate with: the session's own when one was recorded, else the model's. ok is false when neither is known and the caller must fall back to its own heuristic.
func (c *Calibrator) Ratio(ctx context.Context, sessionID, modelID string) (float64, bool) {
	if c == nil || c.state == nil {
		return 0, false
	}
	if id := safeID(sessionID); id != "" {
		if r, ok := c.read(ctx, sessionKey(id)); ok {
			return r, true
		}
	}
	if id := safeID(modelID); id != "" {
		if r, ok := c.read(ctx, modelKey(id)); ok {
			return r, true
		}
	}
	return 0, false
}

// read returns the stored ratio at key. A missing, unreadable or non-positive record reads as "unknown" — the caller then estimates, which is never worse than the fallback it replaces.
func (c *Calibrator) read(ctx context.Context, key string) (float64, bool) {
	raw, err := c.state.Get(ctx, key)
	if err != nil && !errors.Is(err, kv.ErrNotFound) {
		metrics.KVError("token_ratio", metrics.KVFallback)
	}
	if err != nil || len(raw) == 0 {
		return 0, false
	}
	r, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || r <= 0 {
		return 0, false
	}
	return r, true
}

// formatRatio renders the ratio as a plain decimal string — the record holds one number and nothing else, so it stays readable in a kv browser.
func formatRatio(r float64) []byte {
	return []byte(strconv.FormatFloat(r, 'f', -1, 64))
}
