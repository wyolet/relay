package usagelog

import (
	"context"
	"time"
)

// Reader is the read-side counterpart to Emitter. Implementations
// satisfy this against whatever backing store carries usage events —
// JSONL file today, ClickHouse later, in-memory for tests. Consumers
// (HTTP endpoints, CLI) depend on the interface, not the backend.
type Reader interface {
	// Events returns raw events matching q, in reverse-chronological
	// order (newest first), capped at q.Limit.
	Events(ctx context.Context, q EventQuery) ([]Event, error)

	// Summary returns aggregated rows grouped by q.GroupBy. Each row
	// carries totals + latency percentiles over the events matching
	// the filter. Rows are sorted by Requests descending.
	Summary(ctx context.Context, q SummaryQuery) (SummaryResult, error)
}

// EventQuery filters the raw event stream. All fields are optional;
// zero values mean "no filter on this dimension."
type EventQuery struct {
	// Since restricts to events with ts >= now() - Since.
	// Zero means no lower bound (all-time).
	Since time.Duration

	// Optional dimension filters — exact match.
	RelayKeyHash string
	PolicyID     string
	ModelID      string
	HostID       string
	Source       string // "pipeline" | "proxy" | "ws" | "batch"

	// StatusMin / StatusMax restrict by HTTP status. Zero values mean
	// unbounded on that side. StatusMin=400 picks errors only;
	// StatusMin=200, StatusMax=299 picks successes only.
	StatusMin int
	StatusMax int

	// Limit caps the number of events returned. <=0 → DefaultEventLimit.
	Limit int
}

// SummaryQuery is EventQuery + group dimension. The filter fields are
// embedded so reader implementations can share filter logic.
type SummaryQuery struct {
	EventQuery

	// GroupBy is the dimension to group on. Valid values:
	// "relay_key_hash", "policy_id", "model_id", "host_id",
	// "host_key_id", "source". Empty → "source".
	GroupBy string
}

// DefaultEventLimit caps an unbounded Events query so a misconfigured
// caller doesn't try to deserialize the whole log file at once.
const DefaultEventLimit = 100

// MaxEventLimit clamps Limit to avoid OOM on a hostile request.
const MaxEventLimit = 10_000

// SummaryRow is one grouped aggregate. The Group map carries the
// grouping dimension(s) keyed by their column name — single-key today,
// extensible to multi-key grouping later without changing the type.
type SummaryRow struct {
	Group        map[string]string `json:"group"`
	Requests     int64             `json:"requests"`
	ErrorCount   int64             `json:"error_count"`
	Tokens       map[string]int64  `json:"tokens"`
	DurationMs   DurationStats     `json:"duration_ms"`
	FirstSeen    time.Time         `json:"first_seen"`
	LastSeen     time.Time         `json:"last_seen"`
}

// DurationStats holds latency aggregates in milliseconds.
type DurationStats struct {
	Avg int64 `json:"avg"`
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

// SummaryResult wraps the rows with the resolved time range so the
// caller can render "events from X to Y" without re-deriving from
// the query.
type SummaryResult struct {
	Rows []SummaryRow `json:"rows"`
	From time.Time    `json:"from"`
	To   time.Time    `json:"to"`
}

// ValidGroupBy lists the accepted GroupBy values. Used by the HTTP
// endpoint to reject typos with a clear 400 instead of silently
// grouping on nothing.
var ValidGroupBy = []string{
	"relay_key_hash",
	"policy_id",
	"model_id",
	"host_id",
	"host_key_id",
	"source",
}

// IsValidGroupBy reports whether g is one of the supported group
// dimensions.
func IsValidGroupBy(g string) bool {
	for _, v := range ValidGroupBy {
		if g == v {
			return true
		}
	}
	return false
}
