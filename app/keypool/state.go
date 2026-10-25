package keypool

import (
	"encoding/json"
	"time"
)

// CooldownReason describes why a key was placed into cooldown (circuit open).
// The empty string means unknown / legacy record written before this field existed.
type CooldownReason string

const (
	ReasonUpstreamAuthFailed   CooldownReason = "upstream_auth_failed"
	ReasonUpstreamForbidden    CooldownReason = "upstream_forbidden"
	ReasonUpstreamRateLimited  CooldownReason = "upstream_rate_limited"
	ReasonUpstreamServerError  CooldownReason = "upstream_server_error"
	ReasonUpstreamNetworkError CooldownReason = "upstream_network_error"
)

// CircuitRecord is the per-key circuit-breaker state stored under
// "secret_health:<keyHash>" in pkg/state.
type CircuitRecord struct {
	State          CircuitState
	OpenUntil      time.Time
	BackoffStep    int
	LastTransition time.Time
	// Indefinite marks a record written before auth cooldowns were bounded; it reads as an expired open record.
	Indefinite bool
	Reason     CooldownReason
	// Failures counts the server errors since WindowStart that did not open the key yet.
	Failures    int
	WindowStart time.Time
	// ProbeUntil is when a half-open key's probe lease ends; until then other picks skip the key.
	ProbeUntil time.Time
}

// recordWire is the stored form. Times are unix milliseconds so the breaker scripts can compare them server-side.
type recordWire struct {
	State          CircuitState   `json:"state"`
	OpenUntil      int64          `json:"open_until_ms,omitempty"`
	BackoffStep    int            `json:"backoff_step"`
	LastTransition int64          `json:"last_transition_ms,omitempty"`
	Indefinite     bool           `json:"indefinite,omitempty"`
	Reason         CooldownReason `json:"reason,omitempty"`
	Failures       int            `json:"failures,omitempty"`
	WindowStart    int64          `json:"window_start_ms,omitempty"`
	ProbeUntil     int64          `json:"probe_until_ms,omitempty"`
}

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func encodeRecord(r CircuitRecord) ([]byte, error) {
	return json.Marshal(recordWire{
		State:          r.State,
		OpenUntil:      toMillis(r.OpenUntil),
		BackoffStep:    r.BackoffStep,
		LastTransition: toMillis(r.LastTransition),
		Indefinite:     r.Indefinite,
		Reason:         r.Reason,
		Failures:       r.Failures,
		WindowStart:    toMillis(r.WindowStart),
		ProbeUntil:     toMillis(r.ProbeUntil),
	})
}

func decodeRecord(b []byte) (CircuitRecord, error) {
	var w recordWire
	if err := json.Unmarshal(b, &w); err != nil {
		return CircuitRecord{}, err
	}
	return CircuitRecord{
		State:          w.State,
		OpenUntil:      fromMillis(w.OpenUntil),
		BackoffStep:    w.BackoffStep,
		LastTransition: fromMillis(w.LastTransition),
		Indefinite:     w.Indefinite,
		Reason:         w.Reason,
		Failures:       w.Failures,
		WindowStart:    fromMillis(w.WindowStart),
		ProbeUntil:     fromMillis(w.ProbeUntil),
	}, nil
}

// stateName returns a human-readable label for logging.
func stateName(s CircuitState) string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// String returns a stable lower-snake label for the circuit state, suitable
// for logs and API responses.
func (s CircuitState) String() string { return stateName(s) }
