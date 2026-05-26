// Usage read-side endpoints: surface the JSONL stream the post-flight
// observer writes via filtered + aggregated queries. Backed by a
// usagelog.Reader, so the store can swap (file today, ClickHouse later)
// without touching this layer.
//
//	GET /usage/events    raw events, newest first, filterable
//	GET /usage/summary   per-group aggregates over the filtered set
package control

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/usagelog"
)

// --- shared input filters ---

type usageFilterInput struct {
	Since        string   `query:"since" doc:"Relative window (e.g. \"1h\", \"24h\", \"7d\"). Default \"1h\". Ignored when from is set."`
	From         string   `query:"from" doc:"Absolute lower bound (RFC3339). Overrides since."`
	To           string   `query:"to" doc:"Absolute upper bound (RFC3339)."`
	RequestID    string   `query:"request_id" doc:"Exact match on a single request id (deep-link one event)."`
	RelayKeyHash []string `query:"relay_key_hash" doc:"Match any of the given sha256 hashes of the inbound bearer."`
	PolicyID     []string `query:"policy_id" doc:"Match any of the given Policy.metadata.id values."`
	ModelID      []string `query:"model_id" doc:"Match any of the given Model.metadata.id values."`
	HostID       []string `query:"host_id" doc:"Match any of the given Host.metadata.id values."`
	Source       []string `query:"source" doc:"Match any of \"pipeline\" | \"proxy\" | \"ws\" | \"batch\"."`
	FinishReason []string `query:"finish_reason" doc:"Match any of \"stop\" | \"length\" | \"tool_calls\" | \"content_filter\" | \"refusal\"."`
	ErrorKind    []string `query:"error_kind" doc:"Match any of the given error_kind values."`
	StatusMin    int      `query:"status_min" doc:"Minimum HTTP status to include."`
	StatusMax    int      `query:"status_max" doc:"Maximum HTTP status to include."`
}

func (f usageFilterInput) toEventQuery() (usagelog.EventQuery, error) {
	since, err := parseSince(f.Since)
	if err != nil {
		return usagelog.EventQuery{}, err
	}
	from, err := parseTime("from", f.From)
	if err != nil {
		return usagelog.EventQuery{}, err
	}
	to, err := parseTime("to", f.To)
	if err != nil {
		return usagelog.EventQuery{}, err
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return usagelog.EventQuery{}, huma.Error400BadRequest("`to` must not be before `from`")
	}
	return usagelog.EventQuery{
		Since:        since,
		From:         from,
		To:           to,
		RequestID:    f.RequestID,
		RelayKeyHash: f.RelayKeyHash,
		PolicyID:     f.PolicyID,
		ModelID:      f.ModelID,
		HostID:       f.HostID,
		Source:       f.Source,
		FinishReason: f.FinishReason,
		ErrorKind:    f.ErrorKind,
		StatusMin:    f.StatusMin,
		StatusMax:    f.StatusMax,
	}, nil
}

// effectiveWindow returns the wall-clock span the query covers, used to
// guard the time-series bucket count. From/To take precedence over Since.
func effectiveWindow(q usagelog.EventQuery) time.Duration {
	if !q.From.IsZero() {
		end := q.To
		if end.IsZero() {
			end = time.Now()
		}
		return end.Sub(q.From)
	}
	return q.Since
}

// --- /usage/events ---

type usageEventsInput struct {
	usageFilterInput
	Limit int `query:"limit" doc:"Cap on returned events. Default 100, max 10000."`
}

type usageEventsOutput struct {
	Body struct {
		Events []usagelog.Event `json:"events"`
	}
}

// --- /usage/summary ---

type usageSummaryInput struct {
	usageFilterInput
	GroupBy string `query:"group_by" doc:"\"source\" (default) | \"model_id\" | \"host_id\" | \"policy_id\" | \"relay_key_hash\" | \"host_key_id\"."`
}

type usageSummaryOutput struct {
	Body usagelog.SummaryResult
}

// --- /usage/timeseries ---

type usageTimeSeriesInput struct {
	usageFilterInput
	Interval string `query:"interval" doc:"Bucket width (e.g. \"5m\", \"1h\", \"1d\"). Required."`
	GroupBy  string `query:"group_by" doc:"Optional dimension to split series by: \"source\" | \"model_id\" | \"host_id\" | \"policy_id\" | \"relay_key_hash\" | \"host_key_id\". Empty returns a single series."`
}

type usageTimeSeriesOutput struct {
	Body usagelog.TimeSeriesResult
}

// --- registration ---

func registerUsage(api huma.API, d Deps, protect huma.Middlewares) {
	if d.UsageReader == nil {
		return
	}

	huma.Register(api, huma.Operation{
		OperationID: "usage_events",
		Method:      http.MethodGet,
		Path:        "/usage/events",
		Summary:     "List recent usage events (newest first), filterable",
		Description: "Streams the post-flight usage JSONL store with " +
			"optional dimension filters. Returns raw events for inspection " +
			"and ad-hoc analysis; for aggregated views use /usage/summary.",
		Tags:        []string{"usage"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *usageEventsInput) (*usageEventsOutput, error) {
		if err := d.Authz.Authorize(ctx, "usage.events", authz.Resource{Kind: "usage"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		q, err := in.toEventQuery()
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		q.Limit = in.Limit
		events, err := d.UsageReader.Events(ctx, q)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if events == nil {
			events = []usagelog.Event{}
		}
		out := &usageEventsOutput{}
		out.Body.Events = events
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "usage_summary",
		Method:      http.MethodGet,
		Path:        "/usage/summary",
		Summary:     "Aggregated usage rows grouped by a chosen dimension",
		Description: "Filters the post-flight stream, groups by group_by, " +
			"and returns per-group totals (requests, tokens, latency " +
			"percentiles, error count). Rows sorted by request count " +
			"descending.",
		Tags:        []string{"usage"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *usageSummaryInput) (*usageSummaryOutput, error) {
		if err := d.Authz.Authorize(ctx, "usage.summary", authz.Resource{Kind: "usage"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		base, err := in.toEventQuery()
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		q := usagelog.SummaryQuery{EventQuery: base, GroupBy: in.GroupBy}
		res, err := d.UsageReader.Summary(ctx, q)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if res.Rows == nil {
			res.Rows = []usagelog.SummaryRow{}
		}
		return &usageSummaryOutput{Body: res}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "usage_timeseries",
		Method:      http.MethodGet,
		Path:        "/usage/timeseries",
		Summary:     "Time-bucketed usage aggregates for charting",
		Description: "Buckets the filtered stream by `interval` (epoch-aligned) " +
			"and returns per-bucket requests, error_count, and token sums. " +
			"With `group_by` set, returns one series per dimension value for " +
			"stacked charts; empty returns a single series. Empty buckets are " +
			"omitted — zero-fill against the returned from/to range.",
		Tags:        []string{"usage"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *usageTimeSeriesInput) (*usageTimeSeriesOutput, error) {
		if err := d.Authz.Authorize(ctx, "usage.timeseries", authz.Resource{Kind: "usage"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		base, err := in.toEventQuery()
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		interval, err := parseInterval(in.Interval)
		if err != nil {
			return nil, err
		}
		if in.GroupBy != "" && !usagelog.IsValidGroupBy(in.GroupBy) {
			return nil, huma.Error400BadRequest("invalid group_by: " + in.GroupBy)
		}
		// Guard against a tiny interval over a large window producing an
		// unbounded number of buckets. The window is From..To when set,
		// else the relative Since (defaults to 1h).
		if int64(effectiveWindow(base)/interval) > usagelog.MaxBuckets {
			return nil, huma.Error400BadRequest(
				"interval too small for the requested window: would exceed the bucket cap — widen interval or shorten since")
		}
		q := usagelog.TimeSeriesQuery{EventQuery: base, Interval: interval, GroupBy: in.GroupBy}
		res, err := d.UsageReader.TimeSeries(ctx, q)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if res.Rows == nil {
			res.Rows = []usagelog.TimeSeriesRow{}
		}
		return &usageTimeSeriesOutput{Body: res}, nil
	})
}

// parseTime parses an optional RFC3339 timestamp. Empty yields the zero
// time (no bound). field names the param for the error message.
func parseTime(field, raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, huma.Error400BadRequest("invalid `" + field + "` (want RFC3339): " + raw)
	}
	return t, nil
}

// parseInterval parses a required bucket width ("5m", "1h", "1d"). Unlike
// parseSince it has no default — empty or non-positive is a 400.
func parseInterval(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, huma.Error400BadRequest("`interval` is required (e.g. \"1h\", \"1d\")")
	}
	d, err := parseSince(raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, huma.Error400BadRequest("`interval` must be positive")
	}
	return d, nil
}

// parseSince accepts "1h", "24h", "7d", or empty (defaults to 1h).
// Returns the resulting time.Duration. Empty input returns 1h.
func parseSince(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Hour, nil
	}
	// "7d" / "30d" — Go's time.ParseDuration doesn't accept day units.
	if strings.HasSuffix(raw, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(raw, "d") + "h")
		if err != nil {
			return 0, huma.Error400BadRequest("invalid `since` value: " + raw).(error)
		}
		return days * 24, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, huma.Error400BadRequest("invalid `since` value: " + raw).(error)
	}
	return d, nil
}
