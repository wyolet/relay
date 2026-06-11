package usage

import (
	"time"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Event is the canonical per-request usage record. Pure attribution +
// token counts — no cost, no pricing, no derived business metrics.
// Cost computation, billing, and analytics are downstream consumer
// concerns; they read this event and apply their own logic.
//
// It lives in pkg/usage (not app/) so every sink/reader backend
// (file, ClickHouse, valkey, postgres) can consume it as a vendorable
// type without importing app/ — preserving the pkg-purity rule. The
// app/usagelog package re-exports it (type alias) for its lifecycle
// observers.
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
	DurationMs   int64  `json:"duration_ms"` // total: start → response closed
	Streamed     bool   `json:"streamed,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"` // "stop"|"length"|"tool_calls"|"content_filter"|"refusal"
	Attempts     int    `json:"attempts,omitempty"`      // upstream tries (pipeline failover); 0 = not tracked
	ErrorKind    string `json:"error_kind,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	// Upstream is the upstream-leg timing breakdown. Nil when the request
	// never reached upstream (routing/pre-flight failure). The total
	// (start → close) is DurationMs; this adds the finer marks DurationMs
	// can't express. See UpstreamTiming for unit + how to derive TTFT.
	Upstream *sdkusage.UpstreamTiming `json:"upstream,omitempty"`

	// Reasoning is the reasoning span (when the model emitted reasoning
	// content). Nil unless the response was a canonical-observed stream
	// that carried reasoning. Microseconds from start, like Upstream.
	Reasoning *sdkusage.ReasoningTiming `json:"reasoning,omitempty"`

	// Attribution — UUIDs (stable, snapshot-resolvable to slugs at
	// query time). Hash of the inbound bearer is included so the
	// plaintext is never logged.
	RelayKeyHash   string `json:"relay_key_hash,omitempty"`
	PolicyID       string `json:"policy_id,omitempty"`
	ModelID        string `json:"model_id,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"` // model string as the caller sent it
	HostID         string `json:"host_id,omitempty"`
	HostKeyID      string `json:"host_key_id,omitempty"`

	// Token usage as reported by the upstream. Empty on error or when
	// the adapter could not extract.
	Tokens sdkusage.Tokens `json:"tokens,omitempty"`

	// Free-form per-runner tags (client_ip for anonymous proxy, etc.)
	Extras map[string]string `json:"extras,omitempty"`

	// Tags are caller-owned (X-WR-Request-Tags, validated post-flight);
	// Extras stays relay-stamped — the provenance split queries rely on.
	Tags map[string]string `json:"tags,omitempty"`
}

// LogOnly reports whether the event records a request rejected before any
// upstream was reached: status 0 with an ErrorKind set (routing denials,
// auth/proxy gating, key-pool exhaustion). Such events stay visible in
// event listings (the logs view) but are excluded from Summary and
// TimeSeries aggregation — they carry no usage and would otherwise pollute
// stats as phantom unattributed requests.
func (e Event) LogOnly() bool { return e.Status == 0 && e.ErrorKind != "" }
