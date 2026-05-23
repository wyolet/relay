// Usage read-side endpoints: surface the JSONL stream the post-flight
// observer writes via filtered + aggregated queries. Backed by a
// usagelog.Reader, so the store can swap (file today, ClickHouse later)
// without touching this layer.
//
//   GET /usage/events    raw events, newest first, filterable
//   GET /usage/summary   per-group aggregates over the filtered set
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
	Since        string `query:"since" doc:"Time window (e.g. \"1h\", \"24h\", \"7d\"). Default: \"1h\"."`
	RelayKeyHash string `query:"relay_key_hash" doc:"Exact match on the sha256 hash of the inbound bearer."`
	PolicyID     string `query:"policy_id" doc:"Exact match on Policy.metadata.id."`
	ModelID      string `query:"model_id" doc:"Exact match on Model.metadata.id."`
	HostID       string `query:"host_id" doc:"Exact match on Host.metadata.id."`
	Source       string `query:"source" doc:"\"pipeline\" | \"proxy\" | \"ws\" | \"batch\"."`
	StatusMin    int    `query:"status_min" doc:"Minimum HTTP status to include."`
	StatusMax    int    `query:"status_max" doc:"Maximum HTTP status to include."`
}

func (f usageFilterInput) toEventQuery() (usagelog.EventQuery, error) {
	since, err := parseSince(f.Since)
	if err != nil {
		return usagelog.EventQuery{}, err
	}
	return usagelog.EventQuery{
		Since:        since,
		RelayKeyHash: f.RelayKeyHash,
		PolicyID:     f.PolicyID,
		ModelID:      f.ModelID,
		HostID:       f.HostID,
		Source:       f.Source,
		StatusMin:    f.StatusMin,
		StatusMax:    f.StatusMax,
	}, nil
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
