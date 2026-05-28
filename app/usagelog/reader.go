package usagelog

import "github.com/wyolet/relay/sdk/usage"

// Reader is the read-side interface backends implement. Canonical
// definition lives in pkg/usage; re-exported here for the control-plane
// consumers (and cmd/relay-stats) that import usagelog. The concrete
// backends live under pkg/usage/<backend> (file today, clickhouse/valkey
// next).
type Reader = usage.Reader

// Query / result types — re-exported from pkg/usage so callers that only
// import usagelog keep compiling. New code may use pkg/usage directly.
type (
	EventQuery       = usage.EventQuery
	SummaryQuery     = usage.SummaryQuery
	SummaryRow       = usage.SummaryRow
	DurationStats    = usage.DurationStats
	SummaryResult    = usage.SummaryResult
	TimeSeriesQuery  = usage.TimeSeriesQuery
	TimeSeriesPoint  = usage.TimeSeriesPoint
	TimeSeriesRow    = usage.TimeSeriesRow
	TimeSeriesResult = usage.TimeSeriesResult
)

const (
	DefaultEventLimit = usage.DefaultEventLimit
	MaxEventLimit     = usage.MaxEventLimit
	MaxBuckets        = usage.MaxBuckets
)

// ValidGroupBy lists the accepted GroupBy values (see pkg/usage).
var ValidGroupBy = usage.ValidGroupBy

// IsValidGroupBy reports whether g is a supported group dimension.
func IsValidGroupBy(g string) bool { return usage.IsValidGroupBy(g) }
