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
	ReasonUpstreamRateLimited  CooldownReason = "upstream_rate_limited"
	ReasonUpstreamServerError  CooldownReason = "upstream_server_error"
	ReasonUpstreamNetworkError CooldownReason = "upstream_network_error"
	ReasonLocalRateLimited     CooldownReason = "local_rl_exhausted"
)

// circuitRecord is the per-key circuit-breaker state stored under
// "secret_health:<keyHash>" in pkg/state.
type circuitRecord struct {
	State          CircuitState   `json:"state"`
	OpenUntil      time.Time      `json:"open_until,omitempty"`
	BackoffStep    int            `json:"backoff_step"`
	LastTransition time.Time      `json:"last_transition"`
	Indefinite     bool           `json:"indefinite,omitempty"`
	Reason         CooldownReason `json:"reason,omitempty"`
}

func encodeRecord(r circuitRecord) ([]byte, error) {
	return json.Marshal(r)
}

func decodeRecord(b []byte) (circuitRecord, error) {
	var r circuitRecord
	err := json.Unmarshal(b, &r)
	return r, err
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
