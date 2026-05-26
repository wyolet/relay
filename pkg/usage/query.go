package usage

import (
	"context"
	"time"
)

// Sink consumes usage events. Implementations are expected to be
// non-blocking from the Emitter drain goroutine's perspective — slow
// I/O belongs inside the sink's own buffering, not in the caller.
//
// A backend that needs a graceful drain (flush buffered rows, close a
// remote connection) additionally implements Closer; the Emitter calls
// Close after draining at shutdown.
type Sink interface {
	// Write delivers one event. The error is logged by the Emitter;
	// returning non-nil does not stop subsequent events.
	Write(ev Event) error
}

// Closer is the optional shutdown contract for a Sink. The Emitter
// type-asserts each sink for it on Close and, when present, calls Close
// after the queue drains — giving buffered/remote backends (ClickHouse,
// valkey) a chance to flush their final batch before exit.
type Closer interface {
	Close() error
}

// Reader is the read-side counterpart to Sink. Implementations satisfy
// this against whatever backing store carries usage events — JSONL file,
// ClickHouse, valkey, in-memory for tests. Consumers (HTTP endpoints,
// CLI) depend on the interface, not the backend.
type Reader interface {
	// Events returns raw events matching q, in reverse-chronological
	// order (newest first), capped at q.Limit.
	Events(ctx context.Context, q EventQuery) ([]Event, error)

	// Summary returns aggregated rows grouped by q.GroupBy. Each row
	// carries totals + latency percentiles over the events matching
	// the filter. Rows are sorted by Requests descending.
	Summary(ctx context.Context, q SummaryQuery) (SummaryResult, error)

	// TimeSeries returns one or more series of time-bucketed aggregates
	// over the filtered set. With an empty GroupBy a single series is
	// returned; with a GroupBy dimension, one series per distinct value.
	// Buckets are aligned to the Unix epoch by q.Interval and ordered
	// oldest-first within each series.
	TimeSeries(ctx context.Context, q TimeSeriesQuery) (TimeSeriesResult, error)
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
	Group      map[string]string `json:"group"`
	Requests   int64             `json:"requests"`
	ErrorCount int64             `json:"error_count"`
	Tokens     map[string]int64  `json:"tokens"`
	DurationMs DurationStats     `json:"duration_ms"`
	FirstSeen  time.Time         `json:"first_seen"`
	LastSeen   time.Time         `json:"last_seen"`
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

// TimeSeriesQuery is EventQuery + a bucket width + optional group
// dimension. Interval is required (zero is rejected by readers). GroupBy
// is optional: empty yields a single series, a valid dimension yields one
// series per distinct value.
type TimeSeriesQuery struct {
	EventQuery

	// Interval is the bucket width. Must be > 0.
	Interval time.Duration

	// GroupBy splits the result into one series per dimension value.
	// Empty means a single series. Valid values match ValidGroupBy.
	GroupBy string
}

// TimeSeriesPoint is one time bucket's aggregates. Bucket is the bucket's
// start instant (UTC, epoch-aligned). Empty buckets are omitted — the
// frontend zero-fills gaps against the resolved From/To range.
type TimeSeriesPoint struct {
	Bucket     time.Time        `json:"bucket"`
	Requests   int64            `json:"requests"`
	ErrorCount int64            `json:"error_count"`
	Tokens     map[string]int64 `json:"tokens"`
}

// TimeSeriesRow is one series. Group is nil for the single-series case
// and carries the grouping dimension keyed by column name otherwise.
type TimeSeriesRow struct {
	Group  map[string]string `json:"group,omitempty"`
	Points []TimeSeriesPoint `json:"points"`
}

// TimeSeriesResult wraps the series with the resolved interval (echoed as
// a string, e.g. "1h") and time range so the caller can zero-fill and
// label the axis without re-deriving from the query.
type TimeSeriesResult struct {
	Rows     []TimeSeriesRow `json:"rows"`
	Interval string          `json:"interval"`
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
}

// MaxBuckets caps the number of time buckets a single TimeSeries query may
// span (range / interval). Guards against a tiny interval over a huge
// range producing an unbounded result. The HTTP layer rejects with 400
// before hitting a backend.
const MaxBuckets = 5_000

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
