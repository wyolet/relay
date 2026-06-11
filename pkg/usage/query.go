package usage

import (
	"context"
	"strings"
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
	// the filter. Rows are sorted by Requests descending. LogOnly
	// events (pre-upstream rejections) are excluded from aggregation —
	// every backend must apply the Event.LogOnly predicate. The same
	// exclusion applies to TimeSeries; Events listings keep them.
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
	// Time bounds. Lower bound resolution: From if set, else now()-Since,
	// else unbounded. Upper bound: To if set, else unbounded. From/To are
	// absolute and take precedence over the relative Since.
	Since time.Duration
	From  time.Time
	To    time.Time

	// RequestID is an exact single-value lookup (deep-link one event).
	RequestID string

	// Categorical filters — match any value in the slice (SQL IN / set
	// membership). Empty slice means no filter on that dimension.
	RelayKeyHash []string
	PolicyID     []string
	ModelID      []string
	HostID       []string
	Source       []string // "pipeline" | "proxy" | "ws" | "batch"
	FinishReason []string
	ErrorKind    []string

	// StatusMin / StatusMax restrict by HTTP status. Zero values mean
	// unbounded on that side. StatusMin=400 picks errors only;
	// StatusMin=200, StatusMax=299 picks successes only.
	StatusMin int
	StatusMax int
	// Status matches exact HTTP status codes (IN). Empty = no filter.
	// Composes with StatusMin/Max (AND).
	Status []int

	// More categorical filters (set membership; empty = no filter).
	HostKeyID      []string
	RequestedModel []string

	// Slug filters — match the denormalized entity names (Event.Model /
	// Host / Policy / Provider) recorded at event time.
	Model    []string
	Host     []string
	Policy   []string
	Provider []string

	// Tags filters on caller-supplied event tags: key → accepted values.
	// AND across keys, OR within a key's values. An event with the key
	// missing matches only an explicit "" value. Nil/empty = no filter.
	Tags map[string][]string

	// Streamed / ErrorsOnly are tri-state: nil = no filter, else match the
	// bool. ErrorsOnly true matches status>=400 OR error_kind!="" (false
	// matches the complement — "successes only").
	Streamed   *bool
	ErrorsOnly *bool

	// AttemptsMin filters by upstream try count (failover). 0 = no filter.
	AttemptsMin int

	// DurationMsMin / DurationMsMax bound total request duration (ms).
	DurationMsMin int64
	DurationMsMax int64

	// TTFTMsMin / TTFTMsMax bound upstream time-to-first-byte (ms), derived
	// from Upstream.ResponseStart (µs from start). Events with no upstream
	// timing are excluded when either bound is set.
	TTFTMsMin int64
	TTFTMsMax int64

	// Q is a free-text needle matched (case-insensitive substring) against
	// request_id, model_id, requested_model, and source.
	Q string

	// Limit caps the number of events returned. <=0 → DefaultEventLimit.
	Limit int

	// CursorTS / CursorID implement keyset pagination for Events. When
	// CursorTS is non-zero, only events strictly older than the cursor are
	// returned — i.e. (ts, request_id) < (CursorTS, CursorID) under the
	// (ts DESC, request_id DESC) ordering. Set only on the Events path;
	// ignored by Summary / TimeSeries.
	CursorTS time.Time
	CursorID string
}

// SummaryQuery is EventQuery + group dimension. The filter fields are
// embedded so reader implementations can share filter logic.
type SummaryQuery struct {
	EventQuery

	// GroupBy is the dimension to group on. Valid values:
	// "relay_key_hash", "policy_id", "model_id", "host_id",
	// "host_key_id", "source", "finish_reason", "error_kind",
	// "model", "host", "policy", "provider" (event-time slugs),
	// or "tags.<key>" (dynamic, groups on a caller tag's value).
	// Empty → "source".
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
	// TTFTMs aggregates upstream time-to-first-byte (ms, derived from
	// Upstream.ResponseStart) over the subset of events that carry upstream
	// timing. Nil when no event in the group has it — a pointer so "no
	// samples" is distinguishable from "all-zero latency".
	TTFTMs    *DurationStats `json:"ttft_ms,omitempty"`
	FirstSeen time.Time      `json:"first_seen"`
	LastSeen  time.Time      `json:"last_seen"`
	// CostNanos sums Event.CostNanos over the group's priced events
	// (nano-USD). Unpriced counts the events that carried no cost stamp —
	// reported instead of folding them into the sum as silent zeros, so a
	// cost total always says how complete it is.
	CostNanos int64 `json:"cost_nanos"`
	Unpriced  int64 `json:"unpriced"`
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
	Bucket     time.Time `json:"bucket"`
	Requests   int64     `json:"requests"`
	ErrorCount int64     `json:"error_count"`
	// Errors4xx/Errors5xx split ErrorCount by status class for triage
	// charts. They may sum below ErrorCount only if an out-of-band status
	// >= 600 ever appears; in practice ErrorCount = Errors4xx + Errors5xx.
	Errors4xx  int64            `json:"errors_4xx"`
	Errors5xx  int64            `json:"errors_5xx"`
	Tokens     map[string]int64 `json:"tokens"`
	DurationMs DurationStats    `json:"duration_ms"`
	// TTFTMs — see SummaryRow.TTFTMs; nil when no event in the bucket
	// carries upstream timing.
	TTFTMs *DurationStats `json:"ttft_ms,omitempty"`
	// CostNanos / Unpriced — see SummaryRow.
	CostNanos int64 `json:"cost_nanos"`
	Unpriced  int64 `json:"unpriced"`
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

// ValidGroupBy lists the accepted static GroupBy values. Used by the HTTP
// endpoint to reject typos with a clear 400 instead of silently
// grouping on nothing. "tags.<key>" is additionally accepted as a
// dynamic dimension — see TagGroupKey.
var ValidGroupBy = []string{
	"relay_key_hash",
	"policy_id",
	"model_id",
	"host_id",
	"host_key_id",
	"source",
	"finish_reason",
	"error_kind",
	"model",
	"host",
	"policy",
	"provider",
}

// MaxTagKeyLen caps a single tag key. Enforced at write time
// (app/usagelog tag validation) and reused by TagGroupKey so an
// unwritable key can't become a group dimension.
const MaxTagKeyLen = 64

// TagGroupKey extracts the tag key from a dynamic "tags.<key>" group
// dimension. ok is false when g isn't tag-shaped or the key is empty /
// over MaxTagKeyLen. Backends MUST bind the returned key as a query
// parameter, never splice it into SQL text.
func TagGroupKey(g string) (string, bool) {
	key, found := strings.CutPrefix(g, "tags.")
	if !found || key == "" || len(key) > MaxTagKeyLen {
		return "", false
	}
	return key, true
}

// IsValidGroupBy reports whether g is one of the supported group
// dimensions (a ValidGroupBy entry or a "tags.<key>" dynamic one).
func IsValidGroupBy(g string) bool {
	for _, v := range ValidGroupBy {
		if g == v {
			return true
		}
	}
	_, ok := TagGroupKey(g)
	return ok
}
