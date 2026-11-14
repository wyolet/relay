package usage

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

func ptr[T any](v T) *T { return &v }

func ids(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.RequestID)
	}
	return out
}

// A fixed past instant: a relative Since window never reaches it.
var evalBase = time.Date(2020, 6, 11, 12, 0, 0, 0, time.UTC)

// evalEvents differ in every scalar the query filters on; c and d share a timestamp.
func evalEvents() []Event {
	return []Event{
		{RequestID: "a", Timestamp: evalBase, Status: 200, DurationMs: 100, Attempts: 1, Streamed: true,
			Upstream: &sdkusage.UpstreamTiming{ResponseStart: 50_000}, ModelID: "gpt", Source: "pipeline",
			ProjectID: "p-1", Tags: map[string]string{"session": "s1", "leg": "x"}},
		{RequestID: "b", Timestamp: evalBase.Add(time.Minute), Status: 429, ErrorKind: "rate_limited", DurationMs: 20, Attempts: 3,
			RequestedModel: "Claude-Sonnet", RelayKeyHash: "h-1", Tags: map[string]string{"session": "s1"}},
		{RequestID: "c", Timestamp: evalBase.Add(2 * time.Minute), ErrorKind: "no_keys",
			PrincipalID: "u-1", Tags: map[string]string{"session": "s2"}},
		{RequestID: "d", Timestamp: evalBase.Add(2 * time.Minute), Status: 503, DurationMs: 300,
			Upstream: &sdkusage.UpstreamTiming{ResponseStart: 150_999}, Source: "Proxy-GW", ProjectID: "p-2"},
	}
}

func TestFilterEvents(t *testing.T) {
	at := func(d time.Duration) time.Time { return evalBase.Add(d) }
	for _, tc := range []struct {
		name string
		q    EventQuery
		want []string
	}{
		{"no filter", EventQuery{}, []string{"a", "b", "c", "d"}},
		{"from is inclusive", EventQuery{From: at(time.Minute)}, []string{"b", "c", "d"}},
		{"to is inclusive", EventQuery{To: at(time.Minute)}, []string{"a", "b"}},
		{"before is exclusive", EventQuery{Before: at(time.Minute)}, []string{"a"}},
		{"since counts back from now", EventQuery{Since: time.Hour}, []string{}},
		{"from overrides since", EventQuery{From: evalBase, Since: time.Hour}, []string{"a", "b", "c", "d"}},
		{"cursor keeps strictly older rows and smaller ids at its instant", EventQuery{CursorTS: at(2 * time.Minute), CursorID: "d"}, []string{"a", "b", "c"}},
		{"cursor drops its own row", EventQuery{CursorTS: at(2 * time.Minute), CursorID: "c"}, []string{"a", "b"}},
		{"request id", EventQuery{RequestID: "b"}, []string{"b"}},
		{"status min", EventQuery{StatusMin: 400}, []string{"b", "d"}},
		{"status max", EventQuery{StatusMax: 429}, []string{"a", "b", "c"}},
		{"status list", EventQuery{Status: []int{200, 503}}, []string{"a", "d"}},
		{"status list ands with the range", EventQuery{Status: []int{200, 503}, StatusMin: 400}, []string{"d"}},
		{"streamed", EventQuery{Streamed: ptr(true)}, []string{"a"}},
		{"not streamed", EventQuery{Streamed: ptr(false)}, []string{"b", "c", "d"}},
		{"errors only counts a status-0 failure", EventQuery{ErrorsOnly: ptr(true)}, []string{"b", "c", "d"}},
		{"successes only", EventQuery{ErrorsOnly: ptr(false)}, []string{"a"}},
		{"attempts min", EventQuery{AttemptsMin: 2}, []string{"b"}},
		{"duration min is inclusive", EventQuery{DurationMsMin: 100}, []string{"a", "d"}},
		{"duration max is inclusive", EventQuery{DurationMsMax: 100}, []string{"a", "b", "c"}},
		{"ttft min drops events without upstream timing", EventQuery{TTFTMsMin: 51}, []string{"d"}},
		{"ttft max truncates microseconds", EventQuery{TTFTMsMax: 150}, []string{"a", "d"}},
		{"ttft range", EventQuery{TTFTMsMin: 50, TTFTMsMax: 50}, []string{"a"}},
		{"q matches model id ignoring case", EventQuery{Q: "GPT"}, []string{"a"}},
		{"q matches requested model", EventQuery{Q: "sonnet"}, []string{"b"}},
		{"q matches source", EventQuery{Q: "proxy"}, []string{"d"}},
		{"q matches request id", EventQuery{Q: "B"}, []string{"b"}},
		{"tag value", EventQuery{Tags: map[string][]string{"session": {"s1"}}}, []string{"a", "b"}},
		{"tag values or within a key", EventQuery{Tags: map[string][]string{"session": {"s1", "s2"}}}, []string{"a", "b", "c"}},
		{"tag keys and across keys", EventQuery{Tags: map[string][]string{"session": {"s1"}, "leg": {"x"}}}, []string{"a"}},
		{"a missing tag matches the empty value", EventQuery{Tags: map[string][]string{"leg": {""}}}, []string{"b", "c", "d"}},
		{"scope is a disjunction", EventQuery{ScopeProjectID: []string{"p-1"}, ScopeRelayKeyHash: []string{"h-1"}}, []string{"a", "b"}},
		{"scope by principal", EventQuery{ScopePrincipalID: []string{"u-1"}}, []string{"c"}},
		{"a filter narrows the scope, never widens it", EventQuery{ScopeProjectID: []string{"p-1"}, ProjectID: []string{"p-2"}}, []string{}},
		{"an empty scope value matches no unattributed event", EventQuery{ScopeProjectID: []string{""}, ScopeRelayKeyHash: []string{""}, ScopePrincipalID: []string{""}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, ids(FilterEvents(evalEvents(), tc.q))); diff != "" {
				t.Errorf("request ids (-want +got):\n%s", diff)
			}
		})
	}
}

// Every []string filter of EventQuery reads the Event field of the same name: one value keeps its event, several OR, an unknown value keeps nothing.
func TestFilterEventsListFiltersMatchTheirOwnField(t *testing.T) {
	queryType := reflect.TypeFor[EventQuery]()
	checked := 0
	for i := range queryType.NumField() {
		f := queryType.Field(i)
		if f.Type != reflect.TypeFor[[]string]() || len(f.Name) > 5 && f.Name[:5] == "Scope" {
			continue
		}
		checked++
		x, y := Event{RequestID: "x"}, Event{RequestID: "y"}
		reflect.ValueOf(&x).Elem().FieldByName(f.Name).SetString("x-value")
		reflect.ValueOf(&y).Elem().FieldByName(f.Name).SetString("y-value")
		for _, tc := range []struct {
			values []string
			want   []string
		}{
			{[]string{"x-value"}, []string{"x"}},
			{[]string{"y-value", "x-value"}, []string{"x", "y"}},
			{[]string{"other"}, []string{}},
		} {
			var q EventQuery
			reflect.ValueOf(&q).Elem().Field(i).Set(reflect.ValueOf(tc.values))
			if diff := cmp.Diff(tc.want, ids(FilterEvents([]Event{x, y, {RequestID: "unset"}}, q))); diff != "" {
				t.Errorf("%s = %v (-want +got):\n%s", f.Name, tc.values, diff)
			}
		}
	}
	if checked != 22 {
		t.Errorf("checked %d list filters, want 22: a new filter field needs an Event field of the same name", checked)
	}
}

func TestSortAndLimit(t *testing.T) {
	if diff := cmp.Diff([]string{"d", "c", "b", "a"}, ids(SortAndLimit(evalEvents(), 10))); diff != "" {
		t.Errorf("newest first, larger id first at one instant (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"d", "c"}, ids(SortAndLimit(evalEvents(), 2))); diff != "" {
		t.Errorf("limit 2 (-want +got):\n%s", diff)
	}
	many := make([]Event, MaxEventLimit+1)
	for _, tc := range []struct{ limit, want int }{
		{0, DefaultEventLimit},
		{-1, DefaultEventLimit},
		{MaxEventLimit, MaxEventLimit},
		{MaxEventLimit + 1, MaxEventLimit},
	} {
		if got := len(SortAndLimit(many, tc.limit)); got != tc.want {
			t.Errorf("limit %d kept %d events, want %d", tc.limit, got, tc.want)
		}
	}
}

func TestCountEvents(t *testing.T) {
	cursor := EventQuery{CursorTS: evalBase, CursorID: "a"}
	for _, tc := range []struct {
		name  string
		q     EventQuery
		limit int
		want  int64
	}{
		{"all", EventQuery{}, 0, 4},
		{"the cursor is ignored", cursor, 0, 4},
		{"filtered", EventQuery{StatusMin: 400}, 0, 2},
		{"stops at the limit", EventQuery{}, 3, 3},
		{"a limit above the matches", EventQuery{StatusMin: 400}, 5, 2},
	} {
		if got := CountEvents(evalEvents(), tc.q, tc.limit); got != tc.want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Paging by the last row's (timestamp, id) walks every event once, newest first, across a shared instant.
func TestCursorPagingVisitsEveryEventOnce(t *testing.T) {
	var got []string
	q := EventQuery{Limit: 2}
	for {
		page := SortAndLimit(FilterEvents(evalEvents(), q), q.Limit)
		got = append(got, ids(page)...)
		if len(page) < q.Limit || len(got) > 4 {
			break
		}
		q.CursorTS, q.CursorID = page[len(page)-1].Timestamp, page[len(page)-1].RequestID
	}
	if diff := cmp.Diff([]string{"d", "c", "b", "a"}, got); diff != "" {
		t.Errorf("paged ids (-want +got):\n%s", diff)
	}
}
