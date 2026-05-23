package usagelog

import (
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Event is the canonical per-request usage record. Pure attribution +
// token counts — no cost, no pricing, no derived business metrics.
// Cost computation, billing, and analytics are downstream consumer
// concerns; they read this event and apply their own logic.
//
// Field order is fixed (Go marshals in struct order) so JSONL / jq /
// ClickHouse column mapping stays stable across releases.
type Event struct {
	// Identity / trace
	RequestID string    `json:"request_id"`
	Source    string    `json:"source"` // "pipeline" | "proxy" | "ws" | "batch"
	Timestamp time.Time `json:"ts"`

	// Outcome
	Status       int    `json:"status"`
	DurationMs   int64  `json:"duration_ms"`
	ErrorKind    string `json:"error_kind,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	// Attribution — UUIDs (stable, snapshot-resolvable to slugs at
	// query time). Hash of the inbound bearer is included so the
	// plaintext is never logged.
	RelayKeyHash string `json:"relay_key_hash,omitempty"`
	PolicyID     string `json:"policy_id,omitempty"`
	ModelID      string `json:"model_id,omitempty"`
	HostID       string `json:"host_id,omitempty"`
	HostKeyID    string `json:"host_key_id,omitempty"`

	// Token usage as reported by the upstream. Empty on error or when
	// the adapter could not extract.
	Tokens usage.Tokens `json:"tokens,omitempty"`

	// Free-form per-runner tags (client_ip for anonymous proxy, etc.)
	Extras map[string]string `json:"extras,omitempty"`
}
