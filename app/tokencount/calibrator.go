package tokencount

import (
	"context"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

const (
	// sessionTTL outlives a coding session's idle gaps but not the day: a conversation's prompt shape stops being evidence once it is over.
	sessionTTL = 24 * time.Hour

	// modelTTL keeps the coarser fallback across restarts and quiet weekends.
	modelTTL = 7 * 24 * time.Hour
)

// store is the narrow kv surface a Calibrator needs: one ratio per key, last write wins, every key expiring on its own.
type store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Calibrator records and reads the observed tokens-per-byte ratio. Construct once at boot over the shared kv store; safe for concurrent use.
type Calibrator struct {
	state store
}

// NewCalibrator constructs a Calibrator over s.
func NewCalibrator(s kv.Store) *Calibrator { return &Calibrator{state: s} }

// Observe records one true measurement: inputTokens is what the upstream reported for a request whose body was bodyBytes long. Both ids are optional — an empty or unusable one is skipped, so a request with no session still teaches the model ratio. Non-positive inputs are ignored rather than stored as a zero ratio.
//
// Errors are dropped on purpose: a lost sample costs the next estimate some accuracy and nothing else, and this runs in post-flight where there is no caller to tell.
func (c *Calibrator) Observe(ctx context.Context, sessionID, modelID string, bodyBytes, inputTokens int) {
	if c == nil || c.state == nil || bodyBytes <= 0 || inputTokens <= 0 {
		return
	}
	val := formatRatio(float64(inputTokens) / float64(bodyBytes))
	if id := safeID(sessionID); id != "" {
		_ = c.state.Set(ctx, sessionKey(id), val, sessionTTL)
	}
	if id := safeID(modelID); id != "" {
		_ = c.state.Set(ctx, modelKey(id), val, modelTTL)
	}
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
