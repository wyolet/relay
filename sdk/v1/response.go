package v1

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/sdk/usage"
)

// Response is the canonical non-streaming response.
// It carries only what was produced — no request-echo fields.
// Vendor adapters that require echo (e.g. OpenAI Responses) receive the
// original *Request explicitly via SerializeResponse.
type Response struct {
	ID           string       `json:"id"`
	Object       string       `json:"object"`     // always "response"
	CreatedAt    int64        `json:"created_at"` // unix seconds
	Model        string       `json:"model"`
	Status       Status       `json:"status"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Output       []Item       `json:"output,omitempty"`

	// Usage is the orthogonal-meter token map (input, output, cache_read,
	// cache_creation, reasoning, audio_input, audio_output, …). Keys
	// match pricing.MeterForUsageKey so the same vocabulary flows from
	// adapters through observers through pricing without translation.
	// Some keys are sub-breakdowns of a coarser one (reasoning/audio_output
	// ⊂ output) — so a provider total_tokens is input + output, not
	// Tokens.Sum() over the whole map, which would double-count them.
	Usage usage.Tokens `json:"usage,omitempty"`

	Error             *Error             `json:"error,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`

	// RelayUsage is relay-produced observability for this request: token
	// counts, finish reason, timing (TTFT, total), retry attempts. It is
	// the ONE field no vendor adapter ever populates — it's always nil on
	// a normal round-trip, and only set when the caller opts in with
	// `X-WR-Usage: full`. Declared here (a relay-native first-class field,
	// like CacheConfig — see docs/canonical-protocol.md "relay_usage") so
	// the canonical client and schema stay honest about what relay can put
	// on the wire. On vendor-shaped responses relay injects the same JSON
	// as a top-level `relay_usage` key after translation; vendor SDKs
	// ignore it.
	RelayUsage *RelayUsage `json:"relay_usage,omitempty"`

	// Extensions carries vendor-specific or cross-cutting response fields.
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// RelayUsage is the caller-facing slice of relay's per-request telemetry,
// emitted only when echo is requested. Deliberately a public subset of the
// internal usage record — no relay-key hash, policy/host UUIDs, or other
// operator-only attribution.
type RelayUsage struct {
	RequestID    string       `json:"request_id,omitempty"`
	Tokens       usage.Tokens `json:"tokens,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Attempts     int          `json:"attempts,omitempty"` // upstream tries (>1 = failover)
	Streamed     bool         `json:"streamed,omitempty"`
	Timing       *RelayTiming `json:"timing,omitempty"`
}

// RelayTiming is the per-request checkpoint breakdown. All values are
// microseconds elapsed from the request start (the anchor) — the unit
// lives here, on the struct, not in the field names, and every mark is
// anchored to start rather than chained. Derive intervals by subtracting:
//
//	relay pre-overhead = Upstream.Start
//	upstream TTFT      = Upstream.ResponseStart - Upstream.Start
//	stream body time   = Upstream.ResponseEnd   - Upstream.ResponseStart
//	relay tail         = End                    - Upstream.ResponseEnd
type RelayTiming struct {
	Upstream RelayUpstreamTiming `json:"upstream"`
	End      int64               `json:"end"` // start → response ready
}

// RelayUpstreamTiming groups the upstream-leg checkpoints, each µs from
// the request start.
type RelayUpstreamTiming struct {
	Start         int64 `json:"start"`          // start → handed to upstream
	ResponseStart int64 `json:"response_start"` // start → first upstream byte (TTFT)
	ResponseEnd   int64 `json:"response_end"`   // start → upstream done
}

// ParseResponse decodes a canonical response body — the inverse of marshaling
// a *Response. It exists because Output is an interface slice ([]Item) whose
// concrete types are chosen by each item's "type" field, so the stdlib decoder
// can't reconstruct them on its own. Used by canonical clients reading
// /v1/generate responses.
func ParseResponse(body []byte) (*Response, error) {
	var raw struct {
		ID                string                     `json:"id"`
		Object            string                     `json:"object"`
		CreatedAt         int64                      `json:"created_at"`
		Model             string                     `json:"model"`
		Status            Status                     `json:"status"`
		FinishReason      FinishReason               `json:"finish_reason"`
		Output            json.RawMessage            `json:"output"`
		Usage             usage.Tokens               `json:"usage"`
		Error             *Error                     `json:"error"`
		IncompleteDetails *IncompleteDetails         `json:"incomplete_details"`
		RelayUsage        *RelayUsage                `json:"relay_usage"`
		Extensions        map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	resp := &Response{
		ID:                raw.ID,
		Object:            raw.Object,
		CreatedAt:         raw.CreatedAt,
		Model:             raw.Model,
		Status:            raw.Status,
		FinishReason:      raw.FinishReason,
		Usage:             raw.Usage,
		Error:             raw.Error,
		IncompleteDetails: raw.IncompleteDetails,
		RelayUsage:        raw.RelayUsage,
		Extensions:        raw.Extensions,
	}
	if len(raw.Output) > 0 && string(raw.Output) != "null" {
		items, err := unmarshalItems(raw.Output)
		if err != nil {
			return nil, fmt.Errorf("output: %w", err)
		}
		resp.Output = items
	}
	return resp, nil
}

// Error is an API-level error embedded in a response object.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IncompleteDetails explains why a response was not completed.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}
