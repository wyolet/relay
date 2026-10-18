package usagetest

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

type summaryCase struct {
	name string
	q    usage.SummaryQuery
	// percentiles compares P50/P95/P99; set it only when every row's samples are equal, since backends compute percentiles differently.
	percentiles bool
	want        usage.SummaryResult
}

func summaryQuery(groupBy string) usage.SummaryQuery {
	return usage.SummaryQuery{EventQuery: Window(), GroupBy: groupBy}
}

var (
	modelARow = usage.SummaryRow{
		Group: map[string]string{"model": "model-a"}, Requests: 6,
		ErrorCount: 2, Errors4xx: 1, Errors5xx: 1, Throttled: 1,
		Tokens:     map[string]int64{"input": 36, "output": 22, "cache_read": 7},
		DurationMs: flat(200), TTFTMs: ptr(flat(40)),
		FirstSeen: at(0), LastSeen: at(110), CostNanos: 4750, Unpriced: 2,
	}
	modelBRow = usage.SummaryRow{
		Group: map[string]string{"model": "model-b"}, Requests: 4,
		ErrorCount: 3, Errors4xx: 2, Errors5xx: 1, Throttled: 1,
		Tokens:     map[string]int64{"input": 30, "output": 15},
		DurationMs: flat(100), TTFTMs: ptr(flat(41)),
		FirstSeen: at(20), LastSeen: at(95), CostNanos: 2000, Unpriced: 2,
	}
	pipelineRow = usage.SummaryRow{
		Group: map[string]string{"source": "pipeline"}, Requests: 5,
		ErrorCount: 2, Errors4xx: 1, Errors5xx: 1, Throttled: 1,
		Tokens:     map[string]int64{"input": 35, "output": 20, "cache_read": 7},
		DurationMs: mixed(180, 200), TTFTMs: ptr(mixed(40, 41)),
		FirstSeen: at(0), LastSeen: at(110), CostNanos: 4500, Unpriced: 2,
	}
	proxyRow = usage.SummaryRow{
		Group: map[string]string{"source": "proxy"}, Requests: 5,
		ErrorCount: 3, Errors4xx: 2, Errors5xx: 1, Throttled: 1,
		Tokens:     map[string]int64{"input": 31, "output": 17},
		DurationMs: mixed(140, 200),
		FirstSeen:  at(30), LastSeen: at(95), CostNanos: 2250, Unpriced: 2,
	}
)

func withGroup(r usage.SummaryRow, group map[string]string) usage.SummaryRow {
	r.Group = group
	return r
}

var summaryCases = []summaryCase{
	{
		name:        "group by model",
		q:           summaryQuery("model"),
		percentiles: true,
		want:        usage.SummaryResult{Rows: []usage.SummaryRow{modelARow, modelBRow}, From: at(0), To: at(110)},
	},
	{
		name: "group by source",
		q:    summaryQuery("source"),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{pipelineRow, proxyRow}, From: at(0), To: at(110)},
	},
	{
		name: "omitted group_by groups by source",
		q:    summaryQuery(""),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{pipelineRow, proxyRow}, From: at(0), To: at(110)},
	},
	{
		name: "group by none is one row over the whole slice",
		q:    summaryQuery(usage.GroupByNone),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{{
			Group: map[string]string{}, Requests: 10,
			ErrorCount: 5, Errors4xx: 3, Errors5xx: 2, Throttled: 2,
			Tokens:     map[string]int64{"input": 66, "output": 37, "cache_read": 7},
			DurationMs: mixed(160, 200), TTFTMs: ptr(mixed(40, 41)),
			FirstSeen: at(0), LastSeen: at(110), CostNanos: 6750, Unpriced: 4,
		}}, From: at(0), To: at(110)},
	},
	{
		name: "group by none applies the filter first",
		q: func() usage.SummaryQuery {
			q := summaryQuery(usage.GroupByNone)
			q.Model = []string{"model-b"}
			return q
		}(),
		percentiles: true,
		want: usage.SummaryResult{
			Rows: []usage.SummaryRow{withGroup(modelBRow, map[string]string{})}, From: at(20), To: at(95),
		},
	},
	{
		name: "group by none over no events is no rows",
		q: func() usage.SummaryQuery {
			q := summaryQuery(usage.GroupByNone)
			q.From, q.To = Start.AddDate(-1, 0, 0), Start.AddDate(-1, 0, 1)
			return q
		}(),
		want: usage.SummaryResult{},
	},
	{
		name: "group by a tag folds a missing key into the empty value",
		q:    summaryQuery("tags.env"),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			{
				Group: map[string]string{"tags.env": ""}, Requests: 8,
				ErrorCount: 5, Errors4xx: 3, Errors5xx: 2, Throttled: 2,
				Tokens:     map[string]int64{"input": 36, "output": 22},
				DurationMs: mixed(150, 200), TTFTMs: ptr(mixed(40, 41)),
				FirstSeen: at(20), LastSeen: at(110), CostNanos: 2750, Unpriced: 4,
			},
			{
				Group: map[string]string{"tags.env": "prod"}, Requests: 2,
				Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
				DurationMs: flat(200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(0), LastSeen: at(10), CostNanos: 4000,
			},
		}, From: at(0), To: at(110)},
	},
	{
		// key-a averages 166.7ms duration and 41.3ms TTFT (raw µs): a backend that rounds reports 167, one that averages truncated ms reports 40.
		name: "group by relay_key truncates fractional averages",
		q:    summaryQuery("relay_key"),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			{
				Group: map[string]string{"relay_key": "key-b"}, Requests: 6,
				ErrorCount: 4, Errors4xx: 2, Errors5xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 35, "output": 20},
				DurationMs: mixed(150, 200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(30), LastSeen: at(110), CostNanos: 2500, Unpriced: 3,
			},
			{
				Group: map[string]string{"relay_key": "key-a"}, Requests: 3,
				ErrorCount: 1, Errors4xx: 1, Throttled: 1,
				Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
				DurationMs: mixed(166, 200), TTFTMs: ptr(mixed(41, 41)),
				FirstSeen: at(0), LastSeen: at(20), CostNanos: 4000, Unpriced: 1,
			},
			{
				Group: map[string]string{"relay_key": ""}, Requests: 1,
				Tokens:     map[string]int64{"input": 1, "output": 2},
				DurationMs: flat(200),
				FirstSeen:  at(45), LastSeen: at(45), CostNanos: 250,
			},
		}, From: at(0), To: at(110)},
	},
}

func runSummary(t *testing.T, r usage.Reader) {
	for _, tc := range summaryCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Summary(t.Context(), tc.q)
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			for i := 1; i < len(got.Rows); i++ {
				if got.Rows[i].Requests > got.Rows[i-1].Requests {
					t.Errorf("rows not sorted by requests desc: %d after %d", got.Rows[i].Requests, got.Rows[i-1].Requests)
				}
			}
			assertEqual(t, "summary", normalizeSummary(got, tc.percentiles), normalizeSummary(tc.want, tc.percentiles))
		})
	}
}

func normalizeSummary(res usage.SummaryResult, percentiles bool) usage.SummaryResult {
	out := usage.SummaryResult{From: normalizeTime(res.From), To: normalizeTime(res.To)}
	for _, row := range res.Rows {
		row.Tokens = normalizeTokens(row.Tokens)
		row.DurationMs = normalizeStats(row.DurationMs, percentiles)
		row.TTFTMs = normalizeTTFT(row.TTFTMs, percentiles)
		row.FirstSeen, row.LastSeen = normalizeTime(row.FirstSeen), normalizeTime(row.LastSeen)
		out.Rows = append(out.Rows, row)
	}
	sortByGroup(out.Rows, func(r usage.SummaryRow) map[string]string { return r.Group })
	return out
}
