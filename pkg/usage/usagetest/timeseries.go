package usagetest

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

type timeSeriesCase struct {
	name string
	q    usage.TimeSeriesQuery
	// percentiles — see summaryCase.
	percentiles bool
	want        usage.TimeSeriesResult
}

func timeSeriesQuery(groupBy string) usage.TimeSeriesQuery {
	return usage.TimeSeriesQuery{EventQuery: Window(), Interval: time.Hour, GroupBy: groupBy}
}

func hourly(group map[string]string) usage.TimeSeriesResult {
	return usage.TimeSeriesResult{
		Rows: []usage.TimeSeriesRow{{Group: group, Points: []usage.TimeSeriesPoint{
			{
				Bucket: at(0), Requests: 5,
				ErrorCount: 2, Errors4xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 31, "output": 17, "cache_read": 7},
				DurationMs: mixed(160, 200), TTFTMs: ptr(mixed(41, 41)), CostNanos: 4250, Unpriced: 1,
			},
			{
				Bucket: at(60), Requests: 5,
				ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 35, "output": 20},
				DurationMs: mixed(160, 200), TTFTMs: ptr(flat(40)), CostNanos: 2500, Unpriced: 3,
			},
		}}},
		Interval: "1h0m0s", From: at(0), To: at(110),
	}
}

var timeSeriesCases = []timeSeriesCase{
	{
		name: "omitted group_by is one series with no group",
		q:    timeSeriesQuery(""),
		want: hourly(nil),
	},
	{
		name: "group by none is one series with an empty group",
		q:    timeSeriesQuery(usage.GroupByNone),
		want: hourly(map[string]string{}),
	},
	{
		name:        "group by model orders series by total requests",
		q:           timeSeriesQuery("model"),
		percentiles: true,
		want:        hourlyByModel(),
	},
}

func hourlyByModel() usage.TimeSeriesResult {
	return usage.TimeSeriesResult{
		Rows: []usage.TimeSeriesRow{
			{Group: map[string]string{"model": "model-a"}, Points: []usage.TimeSeriesPoint{
				{
					Bucket: at(0), Requests: 3,
					Tokens:     map[string]int64{"input": 31, "output": 17, "cache_read": 7},
					DurationMs: flat(200), TTFTMs: ptr(flat(40)), CostNanos: 4250,
				},
				{
					Bucket: at(60), Requests: 3,
					ErrorCount: 2, Errors4xx: 1, Errors5xx: 1, Throttled: 1,
					Tokens:     map[string]int64{"input": 5, "output": 5},
					DurationMs: flat(200), TTFTMs: ptr(flat(40)), CostNanos: 500, Unpriced: 2,
				},
			}},
			{Group: map[string]string{"model": "model-b"}, Points: []usage.TimeSeriesPoint{
				{
					Bucket: at(0), Requests: 2,
					ErrorCount: 2, Errors4xx: 2, Throttled: 1,
					DurationMs: flat(100), TTFTMs: ptr(flat(41)), Unpriced: 1,
				},
				{
					Bucket: at(60), Requests: 2,
					ErrorCount: 1, Errors5xx: 1,
					Tokens:     map[string]int64{"input": 30, "output": 15},
					DurationMs: flat(100), CostNanos: 2000, Unpriced: 1,
				},
			}},
		},
		Interval: "1h0m0s", From: at(0), To: at(110),
	}
}

func runTimeSeries(t *testing.T, r usage.Reader, cases []timeSeriesCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.TimeSeries(t.Context(), tc.q)
			if err != nil {
				t.Fatalf("TimeSeries: %v", err)
			}
			assertEqual(t, "timeseries", normalizeTimeSeries(got, tc.percentiles), normalizeTimeSeries(tc.want, tc.percentiles))
		})
	}
}

// normalizeTimeSeries keeps row and point order: series order is part of the contract (usage.SortTimeSeriesRows), unlike summary row ties.
func normalizeTimeSeries(res usage.TimeSeriesResult, percentiles bool) usage.TimeSeriesResult {
	out := usage.TimeSeriesResult{Labels: res.Labels, Interval: res.Interval, From: normalizeTime(res.From), To: normalizeTime(res.To)}
	for _, row := range res.Rows {
		points := make([]usage.TimeSeriesPoint, 0, len(row.Points))
		for _, p := range row.Points {
			p.Bucket = normalizeTime(p.Bucket)
			p.Tokens = normalizeTokens(p.Tokens)
			p.DurationMs = normalizeStats(p.DurationMs, percentiles)
			p.TTFTMs = normalizeTTFT(p.TTFTMs, percentiles)
			points = append(points, p)
		}
		out.Rows = append(out.Rows, usage.TimeSeriesRow{Group: row.Group, Points: points})
	}
	return out
}
