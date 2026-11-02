package usagetest

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// runRefusalWindow loads one served request and two refusals from ten minutes ago into a store of their own, and checks every way of naming a window counts the refusals when grouping or faceting by refusal: a relative since, a from alone, and a from/to range.
func runRefusalWindow(t *testing.T, open Open) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	ts := now.Add(-10 * time.Minute)
	served := usage.Event{RequestID: "w-served", Source: "pipeline", Timestamp: ts, Status: 200, DurationMs: 100, ModelID: "mid-a", Model: "model-a"}
	notAllowed := usage.Event{RequestID: "w-denied", Source: "pipeline", Timestamp: ts.Add(time.Second), Status: 403, ErrorKind: "model_not_allowed", Refusal: "model_not_allowed", RequestedModel: "model-b"}
	limited := usage.Event{RequestID: "w-limited", Source: "pipeline", Timestamp: ts.Add(2 * time.Second), Status: 429, ErrorKind: "rate_limited", Refusal: "no_capacity", ModelID: "mid-a", Model: "model-a"}
	r := open(t, []usage.Event{served, notAllowed, limited})

	want := map[string]int64{"": 1, "model_not_allowed": 1, "no_capacity": 1}
	for name, window := range map[string]usage.EventQuery{
		"since":   {Since: time.Hour},
		"from":    {From: now.Add(-time.Hour)},
		"from-to": {From: now.Add(-time.Hour), To: now.Add(time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			sum, err := r.Summary(t.Context(), usage.SummaryQuery{EventQuery: window, GroupBy: "refusal"})
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			got := map[string]int64{}
			for _, row := range sum.Rows {
				got[row.Group["refusal"]] = row.Requests
			}
			assertEqual(t, "summary by refusal", got, want)

			ts, err := r.TimeSeries(t.Context(), usage.TimeSeriesQuery{EventQuery: window, GroupBy: "refusal", Interval: time.Hour})
			if err != nil {
				t.Fatalf("TimeSeries: %v", err)
			}
			got = map[string]int64{}
			for _, row := range ts.Rows {
				for _, p := range row.Points {
					got[row.Group["refusal"]] += p.Requests
				}
			}
			assertEqual(t, "timeseries by refusal", got, want)

			facets, err := usage.Facets(t.Context(), r, window, []string{"refusal"}, 0)
			if err != nil {
				t.Fatalf("Facets: %v", err)
			}
			got = map[string]int64{}
			for _, v := range facets["refusal"] {
				got[v.Value] = v.Requests
			}
			assertEqual(t, "facet refusal", got, want)

			// Every other aggregate keeps counting the served request alone.
			plain, err := r.Summary(t.Context(), usage.SummaryQuery{EventQuery: window, GroupBy: usage.GroupByNone})
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			if len(plain.Rows) != 1 || plain.Rows[0].Requests != 1 {
				t.Fatalf("summary without refusal: %+v, want the one served request", plain.Rows)
			}
		})
	}
}
