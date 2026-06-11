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
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/usagelog"
)

// --- shared input filters ---

type UsageFilterInput struct {
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

	HostKeyID      []string `query:"host_key_id" doc:"Match any of the given HostKey.metadata.id values."`
	RequestedModel []string `query:"requested_model" doc:"Match any of the model strings as the caller sent them."`
	Model          []string `query:"model" doc:"Match any of the given model slugs (event-time metadata.name, denormalized)."`
	Host           []string `query:"host" doc:"Match any of the given host slugs (event-time metadata.name, denormalized)."`
	Policy         []string `query:"policy" doc:"Match any of the given policy slugs (event-time metadata.name, denormalized)."`
	Provider       []string `query:"provider" doc:"Match any of the given provider slugs (event-time, denormalized)."`
	Status         []int    `query:"status" doc:"Match any of these exact HTTP status codes."`
	StatusClass    string   `query:"status_class" doc:"Convenience status band: \"2xx\" | \"4xx\" | \"5xx\". Sets status_min/max."`
	Streamed       string   `query:"streamed" enum:"true,false" doc:"true = only streamed responses, false = only non-streamed."`
	Error          string   `query:"error" enum:"true,false" doc:"true = only errors (status>=400 or error_kind set), false = only successes."`
	AttemptsMin    int      `query:"attempts_min" doc:"Minimum upstream try count (failover) — finds retried requests."`
	DurationMsMin  int64    `query:"duration_ms_min" doc:"Minimum total duration in ms (slow-request filter)."`
	DurationMsMax  int64    `query:"duration_ms_max" doc:"Maximum total duration in ms."`
	TTFTMsMin      int64    `query:"ttft_ms_min" doc:"Minimum upstream time-to-first-byte (ms); excludes requests with no upstream timing."`
	TTFTMsMax      int64    `query:"ttft_ms_max" doc:"Maximum upstream time-to-first-byte (ms)."`
	Q              string   `query:"q" doc:"Free-text substring across request_id, model_id, requested_model, source."`
	Tag            []string `query:"tag" doc:"Caller-tag filter as \"key:value\" (repeatable). Same key repeated matches any of its values; different keys must all match."`
}

func (f UsageFilterInput) toEventQuery() (usagelog.EventQuery, error) {
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

	statusMin, statusMax := f.StatusMin, f.StatusMax
	switch f.StatusClass {
	case "":
		// no band
	case "2xx":
		statusMin, statusMax = 200, 299
	case "4xx":
		statusMin, statusMax = 400, 499
	case "5xx":
		statusMin, statusMax = 500, 599
	default:
		return usagelog.EventQuery{}, huma.Error400BadRequest("`status_class` must be 2xx, 4xx, or 5xx")
	}

	streamed, err := parseTriBool("streamed", f.Streamed)
	if err != nil {
		return usagelog.EventQuery{}, err
	}
	errorsOnly, err := parseTriBool("error", f.Error)
	if err != nil {
		return usagelog.EventQuery{}, err
	}

	tags, err := parseTagFilters(f.Tag)
	if err != nil {
		return usagelog.EventQuery{}, err
	}

	return usagelog.EventQuery{
		Since:          since,
		From:           from,
		To:             to,
		RequestID:      f.RequestID,
		RelayKeyHash:   f.RelayKeyHash,
		PolicyID:       f.PolicyID,
		ModelID:        f.ModelID,
		HostID:         f.HostID,
		Source:         f.Source,
		FinishReason:   f.FinishReason,
		ErrorKind:      f.ErrorKind,
		StatusMin:      statusMin,
		StatusMax:      statusMax,
		Status:         f.Status,
		HostKeyID:      f.HostKeyID,
		RequestedModel: f.RequestedModel,
		Model:          f.Model,
		Host:           f.Host,
		Policy:         f.Policy,
		Provider:       f.Provider,
		Streamed:       streamed,
		ErrorsOnly:     errorsOnly,
		AttemptsMin:    f.AttemptsMin,
		DurationMsMin:  f.DurationMsMin,
		DurationMsMax:  f.DurationMsMax,
		TTFTMsMin:      f.TTFTMsMin,
		TTFTMsMax:      f.TTFTMsMax,
		Q:              f.Q,
		Tags:           tags,
	}, nil
}

// parseTagFilters maps repeated "key:value" tag params to the EventQuery
// Tags filter (key → accepted values). A param without a ':' is a 400.
func parseTagFilters(raw []string) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	tags := make(map[string][]string, len(raw))
	for _, t := range raw {
		k, v, ok := strings.Cut(t, ":")
		if !ok || k == "" {
			return nil, huma.Error400BadRequest("invalid `tag` value (want key:value): " + t)
		}
		tags[k] = append(tags[k], v)
	}
	return tags, nil
}

// parseTriBool maps an optional "true"/"false" query value to a *bool:
// empty → nil (no filter). huma rejects pointer query params, so tri-state
// bools cross the wire as a string and resolve here.
func parseTriBool(name, v string) (*bool, error) {
	switch v {
	case "":
		return nil, nil
	case "true":
		b := true
		return &b, nil
	case "false":
		b := false
		return &b, nil
	default:
		return nil, huma.Error400BadRequest("`" + name + "` must be true or false")
	}
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
	UsageFilterInput
	Limit  int    `query:"limit" doc:"Cap on returned events (page size). Default 100, max 10000."`
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Returns the next (older) page."`
}

type usageEventsOutput struct {
	Body struct {
		Events []usagelog.Event `json:"events"`
		// NextCursor is set when a full page was returned (more may exist);
		// pass it back as ?cursor= to fetch the next page. Empty on the
		// last page.
		NextCursor string `json:"next_cursor,omitempty"`
	}
}

// --- /usage/summary ---

type usageSummaryInput struct {
	UsageFilterInput
	GroupBy string `query:"group_by" doc:"\"source\" (default) | \"model\" | \"host\" | \"policy\" | \"provider\" (event-time slugs) | \"model_id\" | \"host_id\" | \"policy_id\" | \"relay_key_hash\" | \"host_key_id\" | \"finish_reason\" | \"error_kind\" | \"tags.<key>\" (group by a caller tag's value)."`
}

type usageSummaryOutput struct {
	Body usagelog.SummaryResult
}

// --- /usage/timeseries ---

type usageTimeSeriesInput struct {
	UsageFilterInput
	Interval string `query:"interval" doc:"Bucket width (e.g. \"5m\", \"1h\", \"1d\"). Required."`
	GroupBy  string `query:"group_by" doc:"Optional dimension to split series by: \"source\" | \"model\" | \"host\" | \"policy\" | \"provider\" (event-time slugs) | \"model_id\" | \"host_id\" | \"policy_id\" | \"relay_key_hash\" | \"host_key_id\" | \"finish_reason\" | \"error_kind\" | \"tags.<key>\". Empty returns a single series."`
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
		if in.Cursor != "" {
			ts, id, err := decodeCursor(in.Cursor)
			if err != nil {
				return nil, err
			}
			q.CursorTS, q.CursorID = ts, id
		}
		events, err := d.UsageReader.Events(ctx, q)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if events == nil {
			events = []usagelog.Event{}
		}
		out := &usageEventsOutput{}
		out.Body.Events = events
		// A full page implies more rows may exist — hand back a cursor
		// anchored at the oldest (last) returned event.
		if n := len(events); n > 0 && n == effectiveLimit(in.Limit) {
			last := events[n-1]
			out.Body.NextCursor = encodeCursor(last.Timestamp, last.RequestID)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "usage_summary",
		Method:      http.MethodGet,
		Path:        "/usage/summary",
		Summary:     "Aggregated usage rows grouped by a chosen dimension",
		Description: "Filters the post-flight stream, groups by group_by, " +
			"and returns per-group totals (requests, tokens, latency " +
			"percentiles, TTFT percentiles when available, error count). " +
			"Rows sorted by request count " +
			"descending. Requests rejected before reaching an upstream " +
			"(status 0 with an error kind) are excluded — see " +
			"/usage/events or /logs for those.",
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
			"and returns per-bucket requests, error_count (with a 4xx/5xx " +
			"split), token sums, duration_ms percentiles, and ttft_ms " +
			"percentiles (omitted when no event in the bucket has upstream " +
			"timing). " +
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

// effectiveLimit mirrors the reader-side clamp so the handler knows the
// page size actually applied — needed to decide whether a full page (and
// thus a next_cursor) was returned.
func effectiveLimit(limit int) int {
	if limit <= 0 {
		return usagelog.DefaultEventLimit
	}
	if limit > usagelog.MaxEventLimit {
		return usagelog.MaxEventLimit
	}
	return limit
}

// encodeCursor packs (ts, request_id) into an opaque base64url token.
// Nanosecond precision preserves the exact value the store returned, so
// the keyset comparison round-trips exactly.
func encodeCursor(ts time.Time, id string) string {
	raw := strconv.FormatInt(ts.UnixNano(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor. A malformed cursor is a 400.
func decodeCursor(s string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", huma.Error400BadRequest("invalid cursor")
	}
	ns, id, ok := strings.Cut(string(b), ":")
	if !ok {
		return time.Time{}, "", huma.Error400BadRequest("invalid cursor")
	}
	nanos, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, "", huma.Error400BadRequest("invalid cursor")
	}
	return time.Unix(0, nanos).UTC(), id, nil
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
