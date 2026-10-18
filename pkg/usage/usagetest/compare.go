package usagetest

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

// windowQuery covers [at(from), at(to)]. Most cases use 45..105: r05 sits exactly on its start, so a backend that counts it in both windows or in neither is caught.
func windowQuery(groupBy string, from, to int) usage.SummaryQuery {
	return usage.SummaryQuery{EventQuery: usage.EventQuery{From: at(from), To: at(to)}, GroupBy: groupBy}
}

var summaryWithPreviousCases = []summaryCase{
	{
		name:        "group by model attaches each model's previous window",
		q:           windowQuery("model", 45, 105),
		percentiles: true,
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			{
				Group: map[string]string{"model": "model-a"}, Requests: 3,
				ErrorCount: 2, Errors4xx: 1, Errors5xx: 1, Throttled: 1,
				Tokens:     map[string]int64{"input": 1, "output": 2},
				DurationMs: flat(200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(45), LastSeen: at(85), CostNanos: 250, Unpriced: 2,
				Previous: &usage.SummaryPrevious{
					Requests:   2,
					Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
					DurationMs: flat(200), TTFTMs: ptr(flat(40)), CostNanos: 4000,
				},
			},
			{
				Group: map[string]string{"model": "model-b"}, Requests: 2,
				ErrorCount: 1, Errors5xx: 1,
				Tokens:     map[string]int64{"input": 30, "output": 15},
				DurationMs: flat(100),
				FirstSeen:  at(65), LastSeen: at(95), CostNanos: 2000, Unpriced: 1,
				Previous: &usage.SummaryPrevious{
					Requests: 2, ErrorCount: 2, Errors4xx: 2, Throttled: 1,
					DurationMs: flat(100), TTFTMs: ptr(flat(41)), Unpriced: 1,
				},
			},
		}, From: at(45), To: at(95), PreviousFrom: at(-15), PreviousTo: at(45)},
	},
	{
		// key-b has traffic in both windows, "" only in the current one, key-a only in the previous one.
		name: "groups missing from either window",
		q:    windowQuery("relay_key", 45, 105),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{
			{
				Group: map[string]string{"relay_key": "key-b"}, Requests: 4,
				ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 30, "output": 15},
				DurationMs: mixed(150, 200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(65), LastSeen: at(95), CostNanos: 2000, Unpriced: 3,
				Previous: &usage.SummaryPrevious{
					Requests: 1, ErrorCount: 1, Errors4xx: 1, DurationMs: flat(100),
				},
			},
			{
				Group: map[string]string{"relay_key": ""}, Requests: 1,
				Tokens:     map[string]int64{"input": 1, "output": 2},
				DurationMs: flat(200),
				FirstSeen:  at(45), LastSeen: at(45), CostNanos: 250,
			},
		}, From: at(45), To: at(95), PreviousFrom: at(-15), PreviousTo: at(45)},
	},
	{
		name: "group by none",
		q:    windowQuery(usage.GroupByNone, 45, 105),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{{
			Group: map[string]string{}, Requests: 5,
			ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
			Tokens:     map[string]int64{"input": 31, "output": 17},
			DurationMs: mixed(160, 200), TTFTMs: ptr(flat(40)),
			FirstSeen: at(45), LastSeen: at(95), CostNanos: 2250, Unpriced: 3,
			Previous: &usage.SummaryPrevious{
				Requests: 4, ErrorCount: 2, Errors4xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
				DurationMs: mixed(150, 200), TTFTMs: ptr(mixed(41, 41)),
				CostNanos: 4000, Unpriced: 1,
			},
		}}, From: at(45), To: at(95), PreviousFrom: at(-15), PreviousTo: at(45)},
	},
	{
		// One key now (r05 is keyless), key-a and key-b before: the previous window carries its own distinct count.
		name: "distinct counts each window separately",
		q: func() usage.SummaryQuery {
			q := windowQuery(usage.GroupByNone, 45, 105)
			q.Distinct = "relay_key"
			return q
		}(),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{{
			Group: map[string]string{}, Requests: 5,
			ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
			Tokens:     map[string]int64{"input": 31, "output": 17},
			DurationMs: mixed(160, 200), TTFTMs: ptr(flat(40)),
			FirstSeen: at(45), LastSeen: at(95), CostNanos: 2250, Unpriced: 3,
			Distinct: ptr64(1),
			Previous: &usage.SummaryPrevious{
				Requests: 4, ErrorCount: 2, Errors4xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
				DurationMs: mixed(150, 200), TTFTMs: ptr(mixed(41, 41)),
				CostNanos: 4000, Unpriced: 1, Distinct: ptr64(2),
			},
		}}, From: at(45), To: at(95), PreviousFrom: at(-15), PreviousTo: at(45)},
	},
	{
		// The previous window is [at(30), at(45)): r04 on its start is in, r05 on its end belongs to the current window only.
		name: "an event on either previous bound",
		q:    windowQuery(usage.GroupByNone, 45, 60),
		want: usage.SummaryResult{Rows: []usage.SummaryRow{{
			Group: map[string]string{}, Requests: 1,
			Tokens:     map[string]int64{"input": 1, "output": 2},
			DurationMs: flat(200),
			FirstSeen:  at(45), LastSeen: at(45), CostNanos: 250,
			Previous: &usage.SummaryPrevious{
				Requests: 1, ErrorCount: 1, Errors4xx: 1, DurationMs: flat(100),
			},
		}}, From: at(45), To: at(45), PreviousFrom: at(30), PreviousTo: at(45)},
	},
	{
		name: "a window with no current events has no rows",
		q:    windowQuery("model", 120, 180),
		want: usage.SummaryResult{PreviousFrom: at(60), PreviousTo: at(120)},
	},
}

// explicitWindowCase compares q with the caller-picked window [prevFrom, prevTo).
type explicitWindowCase struct {
	summaryCase
	prevFrom, prevTo int
}

var explicitWindowCases = []explicitWindowCase{
	{
		// Half the current window's length. r01 sits on the start and counts; r04 sits on the exclusive end and does not.
		summaryCase: summaryCase{
			name:        "a shorter window with events on both bounds",
			q:           windowQuery("model", 45, 105),
			percentiles: true,
			want: usage.SummaryResult{Rows: []usage.SummaryRow{
				{
					Group: map[string]string{"model": "model-a"}, Requests: 3,
					ErrorCount: 2, Errors4xx: 1, Errors5xx: 1, Throttled: 1,
					Tokens:     map[string]int64{"input": 1, "output": 2},
					DurationMs: flat(200), TTFTMs: ptr(flat(40)),
					FirstSeen: at(45), LastSeen: at(85), CostNanos: 250, Unpriced: 2,
					Previous: &usage.SummaryPrevious{
						Requests:   2,
						Tokens:     map[string]int64{"input": 30, "output": 15, "cache_read": 7},
						DurationMs: flat(200), TTFTMs: ptr(flat(40)), CostNanos: 4000,
					},
				},
				{
					Group: map[string]string{"model": "model-b"}, Requests: 2,
					ErrorCount: 1, Errors5xx: 1,
					Tokens:     map[string]int64{"input": 30, "output": 15},
					DurationMs: flat(100),
					FirstSeen:  at(65), LastSeen: at(95), CostNanos: 2000, Unpriced: 1,
					Previous: &usage.SummaryPrevious{
						Requests: 1, ErrorCount: 1, Errors4xx: 1, Throttled: 1,
						DurationMs: flat(100), TTFTMs: ptr(flat(41)), Unpriced: 1,
					},
				},
			}, From: at(45), To: at(95), PreviousFrom: at(0), PreviousTo: at(30)},
		},
		prevFrom: 0, prevTo: 30,
	},
	{
		// [at(30), at(50)) overlaps the current window: r05 counts in both, because the caller asked for it.
		summaryCase: summaryCase{
			name: "a window overlapping the current one",
			q:    windowQuery(usage.GroupByNone, 45, 105),
			want: usage.SummaryResult{Rows: []usage.SummaryRow{{
				Group: map[string]string{}, Requests: 5,
				ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 31, "output": 17},
				DurationMs: mixed(160, 200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(45), LastSeen: at(95), CostNanos: 2250, Unpriced: 3,
				Previous: &usage.SummaryPrevious{
					Requests: 2, ErrorCount: 1, Errors4xx: 1,
					Tokens:     map[string]int64{"input": 1, "output": 2},
					DurationMs: mixed(150, 200), CostNanos: 250,
				},
			}}, From: at(45), To: at(95), PreviousFrom: at(30), PreviousTo: at(50)},
		},
		prevFrom: 30, prevTo: 50,
	},
	{
		// [at(10), at(50)) holds key-a, key-b and keyless r05; the current window holds key-b and r05 again.
		summaryCase: summaryCase{
			name: "distinct counts the explicit window separately",
			q: func() usage.SummaryQuery {
				q := windowQuery(usage.GroupByNone, 45, 105)
				q.Distinct = "relay_key"
				return q
			}(),
			want: usage.SummaryResult{Rows: []usage.SummaryRow{{
				Group: map[string]string{}, Requests: 5,
				ErrorCount: 3, Errors4xx: 1, Errors5xx: 2, Throttled: 1,
				Tokens:     map[string]int64{"input": 31, "output": 17},
				DurationMs: mixed(160, 200), TTFTMs: ptr(flat(40)),
				FirstSeen: at(45), LastSeen: at(95), CostNanos: 2250, Unpriced: 3,
				Distinct: ptr64(1),
				Previous: &usage.SummaryPrevious{
					Requests: 4, ErrorCount: 2, Errors4xx: 2, Throttled: 1,
					Tokens:     map[string]int64{"input": 21, "output": 12, "cache_read": 7},
					DurationMs: mixed(150, 200), TTFTMs: ptr(mixed(41, 41)),
					CostNanos: 3250, Unpriced: 1, Distinct: ptr64(2),
				},
			}}, From: at(45), To: at(95), PreviousFrom: at(10), PreviousTo: at(50)},
		},
		prevFrom: 10, prevTo: 50,
	},
}

func runSummaryWithPrevious(t *testing.T, r usage.Reader) {
	for _, tc := range summaryWithPreviousCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := usage.SummaryWithPrecedingWindow(t.Context(), r, tc.q)
			if err != nil {
				t.Fatalf("SummaryWithPrecedingWindow: %v", err)
			}
			assertEqual(t, "summary with previous", normalizeSummary(got, tc.percentiles), normalizeSummary(tc.want, tc.percentiles))
		})
	}
	for _, tc := range explicitWindowCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := usage.SummaryWithPrevious(t.Context(), r, tc.q, at(tc.prevFrom), at(tc.prevTo))
			if err != nil {
				t.Fatalf("SummaryWithPrevious: %v", err)
			}
			assertEqual(t, "summary with previous", normalizeSummary(got, tc.percentiles), normalizeSummary(tc.want, tc.percentiles))
		})
	}
}
