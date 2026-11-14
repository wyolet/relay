package usage

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

var aggBase = time.Date(2026, 6, 11, 12, 0, 30, 0, time.UTC)

// aggEvents: four aggregated requests over two hours and two models, then a pre-upstream failure and a relay refusal that only the logs view counts.
func aggEvents() []Event {
	later := aggBase.Add(time.Hour)
	return []Event{
		{RequestID: "a", Timestamp: aggBase, Status: 200, Source: "pipeline", ModelID: "m1", Model: "gpt", RelayKeyHash: "k1",
			Tokens: sdkusage.Tokens{"input": 10, "output": 5}, DurationMs: 100,
			Upstream: &sdkusage.UpstreamTiming{ResponseStart: 50_000}, CostNanos: ptr(int64(5_000_000))},
		// Priced at exactly 0, which is not unpriced; its slug is the model's latest.
		{RequestID: "b", Timestamp: aggBase.Add(10 * time.Second), Status: 200, Source: "pipeline", ModelID: "m1", Model: "gpt-renamed", RelayKeyHash: "k2",
			Tokens: sdkusage.Tokens{"input": 1}, DurationMs: 300,
			Upstream: &sdkusage.UpstreamTiming{ResponseStart: 150_999}, CostNanos: ptr(int64(0))},
		{RequestID: "c", Timestamp: later, Status: 429, Source: "pipeline", ModelID: "m1", RelayKeyHash: "k1", DurationMs: 20},
		{RequestID: "d", Timestamp: later, Status: 502, Source: "proxy", ModelID: "m2", Model: "claude", DurationMs: 40},
		{RequestID: "e", Timestamp: later.Add(time.Hour), ErrorKind: "no_keys", Source: "pipeline", ModelID: "m1"},
		{RequestID: "f", Timestamp: later.Add(time.Hour), Status: 429, Refusal: "no_capacity", ErrorKind: "rate_limit_exceeded", Source: "pipeline", ModelID: "m1"},
	}
}

func flatStats(ms int64) DurationStats {
	return DurationStats{Avg: ms, P50: ms, P95: ms, P99: ms, Max: ms}
}

func TestSummarize(t *testing.T) {
	later := aggBase.Add(time.Hour)
	got, err := SummarizeWithDistinct(aggEvents(), "model_id", "relay_key_hash", false)
	if err != nil {
		t.Fatal(err)
	}
	want := SummaryResult{
		Rows: []SummaryRow{
			{
				Group: map[string]string{"model_id": "m1"}, Requests: 3, ErrorCount: 1, Errors4xx: 1, Throttled: 1,
				Tokens:     map[string]int64{"input": 11, "output": 5},
				DurationMs: DurationStats{Avg: 140, P50: 100, P95: 100, P99: 100, Max: 300},
				// Averaged in microseconds, then truncated: (50000+150999)/2/1000.
				TTFTMs:    &DurationStats{Avg: 100, P50: 50, P95: 50, P99: 50, Max: 150},
				FirstSeen: aggBase, LastSeen: later, CostNanos: 5_000_000, Unpriced: 1, Distinct: ptr(int64(2)),
			},
			{
				Group: map[string]string{"model_id": "m2"}, Requests: 1, ErrorCount: 1, Errors5xx: 1,
				Tokens: map[string]int64{}, DurationMs: flatStats(40),
				FirstSeen: later, LastSeen: later, Unpriced: 1, Distinct: ptr(int64(0)),
			},
		},
		Labels: map[string]string{"m1": "gpt-renamed", "m2": "claude"},
		From:   aggBase, To: later,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("summary by model_id (-want +got):\n%s", diff)
	}
}

// requestsByGroup projects a summary to its group values and request counts, in row order.
func requestsByGroup(res SummaryResult) []string {
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		keys := make([]string, 0, len(r.Group))
		for k, v := range r.Group {
			keys = append(keys, k+"="+v)
		}
		out = append(out, strings.Join(keys, ",")+":"+string(rune('0'+r.Requests)))
	}
	return out
}

func TestSummarizeGrouping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groupBy string
		want    []string
	}{
		{"empty group_by is source", "", []string{"source=pipeline:3", "source=proxy:1"}},
		{"none is one row with an empty group", GroupByNone, []string{":4"}},
		{"refusal counts relay refusals, not other log-only rows", "refusal", []string{"refusal=:4", "refusal=no_capacity:1"}},
		{"error_kind leaves log-only rows out", "error_kind", []string{"error_kind=:4"}},
		{"a tag nobody sent is one empty group", "tags.session", []string{"tags.session=:4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Summarize(aggEvents(), tc.groupBy)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, requestsByGroup(res)); diff != "" {
				t.Errorf("groups (-want +got):\n%s", diff)
			}
			if tc.groupBy != "model_id" && res.Labels != nil {
				t.Errorf("labels = %v, want none for a dimension without a slug column", res.Labels)
			}
			if res.Rows[0].Distinct != nil {
				t.Errorf("distinct = %d without a distinct dimension", *res.Rows[0].Distinct)
			}
		})
	}
	if res, err := Summarize(nil, "model"); err != nil || len(res.Rows) != 0 || !res.From.IsZero() || !res.To.IsZero() {
		t.Errorf("no events: %+v, %v; want no rows and a zero window", res, err)
	}
}

func TestSummarizeOrdersEqualCountsStably(t *testing.T) {
	var events []Event
	for _, m := range []string{"a", "b", "c", "d", "e", "f"} {
		events = append(events, Event{RequestID: m, Status: 200, Model: m})
	}
	first, err := Summarize(events, "model")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, _ := Summarize(events, "model")
		if diff := cmp.Diff(requestsByGroup(first), requestsByGroup(again)); diff != "" {
			t.Fatalf("row order changed between calls (-first +again):\n%s", diff)
		}
	}
}

// Every group dimension reads the Event field it is named after.
func TestGroupDimensionsReadTheirOwnField(t *testing.T) {
	field := func(dim string) string {
		var b strings.Builder
		for _, part := range strings.Split(dim, "_") {
			if part == "id" {
				b.WriteString("ID")
				continue
			}
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
		return b.String()
	}
	for _, dim := range ValidGroupBy {
		ev := Event{Status: 200}
		reflect.ValueOf(&ev).Elem().FieldByName(field(dim)).SetString("value")
		res, err := SummarizeWithDistinct([]Event{ev}, dim, "", true)
		if err != nil {
			t.Errorf("%s: %v", dim, err)
			continue
		}
		if diff := cmp.Diff([]string{dim + "=value:1"}, requestsByGroup(res)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", dim, diff)
		}
		top, err := TopValues([]Event{ev}, dim, 0, true)
		if diff := cmp.Diff([]FacetValue{{Value: "value", Requests: 1}}, top); diff != "" || err != nil {
			t.Errorf("TopValues(%s): %v (-want +got):\n%s", dim, err, diff)
		}
	}
	ev := Event{Status: 200, Tags: map[string]string{"k": "tag"}, Extras: map[string]string{"k": "extra"}}
	for dim, want := range map[string]string{"tags.k": "tags.k=tag:1", "extras.k": "extras.k=extra:1"} {
		res, err := Summarize([]Event{ev}, dim)
		if diff := cmp.Diff([]string{want}, requestsByGroup(res)); diff != "" || err != nil {
			t.Errorf("%s: %v (-want +got):\n%s", dim, err, diff)
		}
	}
}

func TestBucketize(t *testing.T) {
	noon, one := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC)
	got, err := BucketizeWithDistinct(aggEvents(), time.Hour, "model_id", "relay_key_hash", false)
	if err != nil {
		t.Fatal(err)
	}
	want := TimeSeriesResult{
		Rows: []TimeSeriesRow{
			{Group: map[string]string{"model_id": "m1"}, Points: []TimeSeriesPoint{
				{Bucket: noon, Requests: 2, Tokens: map[string]int64{"input": 11, "output": 5},
					DurationMs: DurationStats{Avg: 200, P50: 100, P95: 100, P99: 100, Max: 300},
					TTFTMs:     &DurationStats{Avg: 100, P50: 50, P95: 50, P99: 50, Max: 150},
					CostNanos:  5_000_000, Distinct: ptr(int64(2))},
				{Bucket: one, Requests: 1, ErrorCount: 1, Errors4xx: 1, Throttled: 1, Tokens: map[string]int64{},
					DurationMs: flatStats(20), Unpriced: 1, Distinct: ptr(int64(1))},
			}},
			{Group: map[string]string{"model_id": "m2"}, Points: []TimeSeriesPoint{
				{Bucket: one, Requests: 1, ErrorCount: 1, Errors5xx: 1, Tokens: map[string]int64{},
					DurationMs: flatStats(40), Unpriced: 1, Distinct: ptr(int64(0))},
			}},
		},
		Labels: map[string]string{"m1": "gpt-renamed", "m2": "claude"},
		From:   aggBase, To: aggBase.Add(time.Hour),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("hourly series by model_id (-want +got):\n%s", diff)
	}
}

// requestsByBucket projects a series set to "group@HH:MM:SS=requests" per point, in row and point order.
func requestsByBucket(res TimeSeriesResult, groupBy string) []string {
	var out []string
	for _, row := range res.Rows {
		for _, p := range row.Points {
			out = append(out, row.Group[groupBy]+"@"+p.Bucket.Format(time.TimeOnly)+"="+string(rune('0'+p.Requests)))
		}
	}
	return out
}

func TestBucketizeBucketsAndSeriesOrder(t *testing.T) {
	at := func(clock string) time.Time {
		ts, err := time.Parse(time.DateTime, "2026-06-11 "+clock)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	events := []Event{
		{Timestamp: at("12:14:59"), Status: 200, Model: "b"},
		{Timestamp: at("12:15:00"), Status: 200, Model: "b"},
		{Timestamp: at("12:00:00"), Status: 200, Model: "a"},
		{Timestamp: at("12:29:59"), Status: 200, Model: "a"},
		{Timestamp: at("12:31:00"), Status: 200, Model: "c"},
		{Timestamp: at("12:31:00"), Status: 200, Model: "c"},
		{Timestamp: at("12:46:00"), Status: 200, Model: "c"},
	}
	for _, tc := range []struct {
		name     string
		interval time.Duration
		groupBy  string
		want     []string
	}{
		{"one series without group_by, oldest bucket first", 15 * time.Minute, "",
			[]string{"@12:00:00=2", "@12:15:00=2", "@12:30:00=2", "@12:45:00=1"}},
		{"none is one series too", 15 * time.Minute, GroupByNone,
			[]string{"@12:00:00=2", "@12:15:00=2", "@12:30:00=2", "@12:45:00=1"}},
		{"series by total requests, then by group value", 15 * time.Minute, "model",
			[]string{"c@12:30:00=2", "c@12:45:00=1", "a@12:00:00=1", "a@12:15:00=1", "b@12:00:00=1", "b@12:15:00=1"}},
		{"buckets align to the epoch, not to the first event", 7 * time.Minute, "model",
			[]string{"c@12:29:00=2", "c@12:43:00=1", "a@11:54:00=1", "a@12:29:00=1", "b@12:08:00=1", "b@12:15:00=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Bucketize(events, tc.interval, tc.groupBy)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, requestsByBucket(res, tc.groupBy)); diff != "" {
				t.Errorf("points (-want +got):\n%s", diff)
			}
			switch tc.groupBy {
			case "":
				if res.Rows[0].Group != nil {
					t.Errorf("group = %v, want nil without group_by", res.Rows[0].Group)
				}
			case GroupByNone:
				if res.Rows[0].Group == nil || len(res.Rows[0].Group) != 0 {
					t.Errorf("group = %v, want an empty map for none", res.Rows[0].Group)
				}
			}
		})
	}
}

func TestAggregateRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func() error
		want string
	}{
		{"summarize unknown group_by", func() error { _, err := Summarize(nil, "nonsense"); return err }, `usage.Summarize: invalid group_by "nonsense"`},
		{"summarize empty tag key", func() error { _, err := Summarize(nil, "tags."); return err }, `usage.Summarize: invalid group_by "tags."`},
		{"summarize unknown distinct", func() error { _, err := SummarizeWithDistinct(nil, "model", "source", false); return err }, `usage.Summarize: invalid distinct "source"`},
		{"bucketize zero interval", func() error { _, err := Bucketize(nil, 0, ""); return err }, "usage.Bucketize: interval must be > 0"},
		{"bucketize sub-second interval", func() error { _, err := Bucketize(nil, 999*time.Millisecond, ""); return err }, "usage.Bucketize: interval must be >= 1s"},
		{"bucketize unknown group_by", func() error { _, err := Bucketize(nil, time.Hour, "nonsense"); return err }, `usage.Bucketize: invalid group_by "nonsense"`},
		{"bucketize unknown distinct", func() error { _, err := BucketizeWithDistinct(nil, time.Hour, "", "source", false); return err }, `usage.Bucketize: invalid distinct "source"`},
		{"top values of none", func() error { _, err := TopValues(nil, GroupByNone, 0, false); return err }, `usage.TopValues: invalid dim "none"`},
	} {
		if err := tc.run(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestDurationStats(t *testing.T) {
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(100 - i) // 100..1, unsorted
	}
	for _, tc := range []struct {
		name    string
		samples []int64
		want    DurationStats
	}{
		{"none", nil, DurationStats{}},
		{"one", []int64{7}, flatStats(7)},
		{"two take the lower as every percentile", []int64{9, 2}, DurationStats{Avg: 5, P50: 2, P95: 2, P99: 2, Max: 9}},
		{"1..100", samples, DurationStats{Avg: 50, P50: 50, P95: 95, P99: 99, Max: 100}},
	} {
		if diff := cmp.Diff(tc.want, durationStats(tc.samples)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
	if samples[0] != 100 {
		t.Error("durationStats sorted the caller's slice")
	}
	if got := ttftStats(nil); got != nil {
		t.Errorf("ttftStats(nil) = %+v, want nil", got)
	}
}

func TestTopValues(t *testing.T) {
	got, err := TopValues(aggEvents(), "model_id", 0, false)
	if diff := cmp.Diff([]FacetValue{{Value: "m1", Label: "gpt-renamed", Requests: 3}, {Value: "m2", Label: "claude", Requests: 1}}, got); diff != "" || err != nil {
		t.Errorf("model_id: %v (-want +got):\n%s", err, diff)
	}
	got, err = TopValues(aggEvents(), "source", 1, true)
	if diff := cmp.Diff([]FacetValue{{Value: "pipeline", Requests: 4}}, got); diff != "" || err != nil {
		t.Errorf("source with refusals, limit 1: %v (-want +got):\n%s", err, diff)
	}
	// Equal counts order by value.
	got, err = TopValues(aggEvents(), "relay_key_hash", 0, false)
	if diff := cmp.Diff([]FacetValue{{Value: "k1", Requests: 2}, {Value: "", Requests: 1}, {Value: "k2", Requests: 1}}, got); diff != "" || err != nil {
		t.Errorf("relay_key_hash: %v (-want +got):\n%s", err, diff)
	}
	for limit, want := range map[int]int{0: DefaultFacetLimit, -3: DefaultFacetLimit, 7: 7, MaxFacetLimit + 1: MaxFacetLimit} {
		if got := FacetLimit(limit); got != want {
			t.Errorf("FacetLimit(%d) = %d, want %d", limit, got, want)
		}
	}
}
