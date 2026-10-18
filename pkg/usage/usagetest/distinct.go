package usagetest

import "github.com/wyolet/relay/pkg/usage"

func summaryDistinctQuery(groupBy, distinct string) usage.SummaryQuery {
	q := summaryQuery(groupBy)
	q.Distinct = distinct
	return q
}

func timeSeriesDistinctQuery(groupBy, distinct string) usage.TimeSeriesQuery {
	q := timeSeriesQuery(groupBy)
	q.Distinct = distinct
	return q
}

func rowWithDistinct(r usage.SummaryRow, n int64) usage.SummaryRow {
	r.Distinct = &n
	return r
}

// pointsWithDistinct sets Distinct on res's points in row-then-point order; res must come fresh from a constructor so no expected value is shared.
func pointsWithDistinct(res usage.TimeSeriesResult, counts ...int64) usage.TimeSeriesResult {
	i := 0
	for _, row := range res.Rows {
		for p := range row.Points {
			row.Points[p].Distinct = &counts[i]
			i++
		}
	}
	return res
}

var summaryDistinctCases = []summaryCase{
	{
		// proxy holds key-b and keyless r05: the empty value is not a key.
		name: "distinct relay_key per group skips the empty value",
		q:    summaryDistinctQuery("source", "relay_key"),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			rowWithDistinct(pipelineRow, 2), rowWithDistinct(proxyRow, 1),
		}, From: at(0), To: at(110)},
	},
	{
		name: "distinct project_id is zero for a group with no attributed events",
		q:    summaryDistinctQuery("source", "project_id"),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			rowWithDistinct(pipelineRow, 1), rowWithDistinct(proxyRow, 0),
		}, From: at(0), To: at(110)},
	},
	{
		name: "only keyless traffic counts zero keys",
		q: func() usage.SummaryQuery {
			q := summaryDistinctQuery(usage.GroupByNone, "relay_key")
			q.RelayKey = []string{""}
			return q
		}(),
		percentiles: true,
		want: usage.SummaryResult{Rows: []usage.SummaryRow{{
			Group: map[string]string{}, Requests: 1,
			Tokens:     map[string]int64{"input": 1, "output": 2},
			DurationMs: flat(200),
			FirstSeen:  at(45), LastSeen: at(45), CostNanos: 250,
			Distinct: ptr64(0),
		}}, From: at(45), To: at(45)},
	},
}

var timeSeriesDistinctCases = []timeSeriesCase{
	{
		// Two keys in the window, but 2 + 1 per bucket: points do not sum to a window total.
		name: "distinct relay_key per bucket",
		q:    timeSeriesDistinctQuery("", "relay_key"),
		want: pointsWithDistinct(hourly(nil), 2, 1),
	},
	{
		name:        "distinct relay_key per bucket within each series",
		q:           timeSeriesDistinctQuery("model", "relay_key"),
		percentiles: true,
		want:        pointsWithDistinct(hourlyByModel(), 1, 1, 2, 1),
	},
}

func ptr64(n int64) *int64 { return &n }
