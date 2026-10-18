package usagetest

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// labelsCase checks the Labels map Summary and TimeSeries return for one grouping; both must agree, since a series label spans every bucket.
type labelsCase struct {
	name    string
	q       usage.EventQuery
	groupBy string
	want    map[string]string
}

var labelsCases = []labelsCase{
	{
		// hid-1 is host-old in the first hour and host-new in the second, its newest event has no slug and its log-only one is ignored.
		name:    "an id group is labelled with its latest non-empty slug",
		q:       Window(),
		groupBy: "host_id",
		want:    map[string]string{"hid-1": "host-new", "hid-2": "host-b"},
	},
	{
		name: "the label is the latest slug inside the window",
		q: func() usage.EventQuery {
			q := Window()
			q.To = at(30)
			return q
		}(),
		groupBy: "host_id",
		want:    map[string]string{"hid-1": "host-old", "hid-2": "host-b"},
	},
	{
		name:    "a relay key hash is labelled with the key's slug",
		q:       Window(),
		groupBy: "relay_key_hash",
		want:    map[string]string{"hash-a": "key-a", "hash-b": "key-b"},
	},
	{
		name:    "a key credential is labelled with the key's slug",
		q:       Window(),
		groupBy: "credential_id",
		want:    map[string]string{"cred-a": "key-a", "cred-b": "key-b"},
	},
	{name: "a slug dimension has no labels", q: Window(), groupBy: "host"},
	{name: "an id without a slug column has no labels", q: Window(), groupBy: "host_key_id"},
	{name: "group by none has no labels", q: Window(), groupBy: usage.GroupByNone},
}

// runLabelsWithDistinctAndCompare checks that labels ride the same aggregate as distinct counts, and that a comparison keeps the requested window's labels.
func runLabelsWithDistinctAndCompare(t *testing.T, r usage.Reader) {
	t.Run("labels and distinct in one summary", func(t *testing.T) {
		got, err := r.Summary(t.Context(), usage.SummaryQuery{EventQuery: Window(), GroupBy: "host_id", Distinct: "relay_key"})
		if err != nil {
			t.Fatalf("Summary: %v", err)
		}
		assertEqual(t, "labels", got.Labels, map[string]string{"hid-1": "host-new", "hid-2": "host-b"})
		distinct := map[string]int64{}
		for _, row := range got.Rows {
			if row.Distinct == nil {
				t.Fatalf("row %v has no distinct count", row.Group)
			}
			distinct[row.Group["host_id"]] = *row.Distinct
		}
		assertEqual(t, "distinct", distinct, map[string]int64{"hid-1": 2, "hid-2": 2, "hid-3": 0, "": 1})
	})
	t.Run("labels and distinct in one timeseries", func(t *testing.T) {
		got, err := r.TimeSeries(t.Context(), usage.TimeSeriesQuery{EventQuery: Window(), Interval: time.Hour, GroupBy: "host_id", Distinct: "relay_key"})
		if err != nil {
			t.Fatalf("TimeSeries: %v", err)
		}
		assertEqual(t, "labels", got.Labels, map[string]string{"hid-1": "host-new", "hid-2": "host-b"})
		for _, row := range got.Rows {
			for _, p := range row.Points {
				if p.Distinct == nil {
					t.Fatalf("series %v bucket %v has no distinct count", row.Group, p.Bucket)
				}
			}
		}
	})
	// hid-1 is host-old only in the preceding window, so a label taken from it would read host-old.
	t.Run("a comparison keeps the requested window's labels", func(t *testing.T) {
		q := usage.SummaryQuery{EventQuery: usage.EventQuery{From: at(45), To: at(105)}, GroupBy: "host_id", Distinct: "relay_key"}
		got, err := usage.SummaryWithPrecedingWindow(t.Context(), r, q)
		if err != nil {
			t.Fatalf("SummaryWithPrecedingWindow: %v", err)
		}
		assertEqual(t, "labels", got.Labels, map[string]string{"hid-1": "host-new", "hid-2": "host-b"})
		for _, row := range got.Rows {
			if row.Group["host_id"] != "hid-1" {
				continue
			}
			if row.Distinct == nil || row.Previous == nil || row.Previous.Requests != 2 {
				t.Fatalf("hid-1 row = %+v, want a distinct count and 2 previous requests", row)
			}
			return
		}
		t.Fatal("no hid-1 row")
	})
}

func runLabels(t *testing.T, r usage.Reader) {
	for _, tc := range labelsCases {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := r.Summary(t.Context(), usage.SummaryQuery{EventQuery: tc.q, GroupBy: tc.groupBy})
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			assertEqual(t, "summary labels", sum.Labels, tc.want)
			ts, err := r.TimeSeries(t.Context(), usage.TimeSeriesQuery{EventQuery: tc.q, Interval: time.Hour, GroupBy: tc.groupBy})
			if err != nil {
				t.Fatalf("TimeSeries: %v", err)
			}
			assertEqual(t, "timeseries labels", ts.Labels, tc.want)
		})
	}
}
