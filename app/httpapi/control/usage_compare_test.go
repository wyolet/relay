package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/usage"
)

// filteringUsageReader applies each Summary query to its events, and is safe for the two concurrent queries a comparison makes.
type filteringUsageReader struct {
	mu      sync.Mutex
	events  []usagelog.Event
	queries []usagelog.SummaryQuery
}

func (f *filteringUsageReader) Events(context.Context, usagelog.EventQuery) ([]usagelog.Event, error) {
	return nil, nil
}

func (f *filteringUsageReader) Count(context.Context, usagelog.EventQuery, int) (int64, error) {
	return 0, nil
}

func (f *filteringUsageReader) Summary(_ context.Context, q usagelog.SummaryQuery) (usagelog.SummaryResult, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return usage.Summarize(usage.FilterEvents(f.events, q.EventQuery), q.GroupBy)
}

func (f *filteringUsageReader) TimeSeries(context.Context, usagelog.TimeSeriesQuery) (usagelog.TimeSeriesResult, error) {
	return usagelog.TimeSeriesResult{}, nil
}

func (f *filteringUsageReader) Facet(context.Context, usagelog.FacetQuery) ([]usagelog.FacetValue, error) {
	return nil, nil
}

func newCompareHarness(t *testing.T, authzr authz.Authorizer, reader usagelog.Reader) http.Handler {
	t.Helper()
	r, api := testAPI(t, scopeActors)
	registerUsage(api, Deps{Authz: authzr, UsageReader: reader}, nil)
	return r
}

type compareBody struct {
	Rows []struct {
		Group    map[string]string         `json:"group"`
		Requests int64                     `json:"requests"`
		Previous *usagelog.SummaryPrevious `json:"previous"`
	} `json:"rows"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	PreviousFrom time.Time `json:"previous_from"`
	PreviousTo   time.Time `json:"previous_to"`
}

func decodeCompare(t *testing.T, raw []byte) compareBody {
	t.Helper()
	var out compareBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUsageSummaryComparePrevious(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	reader := &filteringUsageReader{events: []usagelog.Event{
		{RequestID: "p-1", Model: "m-a", Timestamp: start.Add(-30 * time.Minute), Status: 200},
		{RequestID: "p-2", Model: "m-gone", Timestamp: start.Add(-20 * time.Minute), Status: 200},
		{RequestID: "c-1", Model: "m-a", Timestamp: start, Status: 200},
		{RequestID: "c-2", Model: "m-a", Timestamp: start.Add(10 * time.Minute), Status: 503},
		{RequestID: "c-3", Model: "m-new", Timestamp: start.Add(20 * time.Minute), Status: 200},
	}}
	h := newCompareHarness(t, testRBAC(), reader)
	w := scopeReq(t, h, "root", http.MethodGet,
		"/usage/summary?group_by=model&compare=previous&from=2026-03-01T12:00:00Z&to=2026-03-01T13:00:00Z", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	out := decodeCompare(t, w.Body.Bytes())
	if len(reader.queries) != 2 {
		t.Fatalf("store queries = %d, want 2", len(reader.queries))
	}
	if !out.PreviousFrom.Equal(start.Add(-time.Hour)) || !out.PreviousTo.Equal(start) {
		t.Fatalf("previous window = %v..%v", out.PreviousFrom, out.PreviousTo)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("rows = %d, want the 2 current-window models: %s", len(out.Rows), w.Body)
	}
	for _, row := range out.Rows {
		switch row.Group["model"] {
		case "m-a":
			if row.Requests != 2 || row.Previous == nil || row.Previous.Requests != 1 {
				t.Errorf("m-a = %d requests, previous %+v; want 2 and 1", row.Requests, row.Previous)
			}
		case "m-new":
			if row.Previous != nil {
				t.Errorf("m-new previous = %+v, want none", row.Previous)
			}
		default:
			t.Errorf("unexpected row %v", row.Group)
		}
	}
}

func TestUsageSummaryOmitsThePreviousWindowWithoutCompare(t *testing.T) {
	reader := &filteringUsageReader{events: []usagelog.Event{{RequestID: "r-1", Timestamp: time.Now(), Status: 200}}}
	h := newCompareHarness(t, testRBAC(), reader)
	w := scopeReq(t, h, "root", http.MethodGet, "/usage/summary", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["previous_from"]; ok {
		t.Fatalf("previous_from present without compare: %s", w.Body)
	}
	if len(reader.queries) != 1 {
		t.Fatalf("store queries = %d, want 1", len(reader.queries))
	}
}

func TestUsageSummaryRejectsABadCompare(t *testing.T) {
	const (
		alternatives = "`compare` and `compare_from`/`compare_to` are alternatives; give one"
		together     = "`compare_from` and `compare_to` must be given together"
		after        = "`compare_to` must be after `compare_from`"
	)
	for query, want := range map[string]string{
		"compare=yesterday":         "`compare` must be \"previous\"",
		"compare=previous&since=0s": usage.ErrNoPreviousWindow.Error(),
		"compare=previous&compare_from=2026-03-01T00:00:00Z&compare_to=2026-03-02T00:00:00Z": alternatives,
		"compare=previous&compare_to=2026-03-02T00:00:00Z":                                   alternatives,
		"compare_from=2026-03-01T00:00:00Z":                                                  together,
		"compare_to=2026-03-02T00:00:00Z":                                                    together,
		"compare_from=2026-03-02T00:00:00Z&compare_to=2026-03-02T00:00:00Z":                  after,
		"compare_from=2026-03-02T00:00:00Z&compare_to=2026-03-01T00:00:00Z":                  after,
		"compare_from=yesterday&compare_to=2026-03-01T00:00:00Z":                             "invalid `compare_from` (want RFC3339): yesterday",
		"compare_from=2026-03-01T00:00:00Z&compare_to=later":                                 "invalid `compare_to` (want RFC3339): later",
	} {
		reader := &filteringUsageReader{}
		w := scopeReq(t, newCompareHarness(t, testRBAC(), reader), "root", http.MethodGet, "/usage/summary?"+query, "")
		var got struct {
			Error struct{ Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 400 || got.Error.Message != want {
			t.Errorf("%s: %d %q, want 400 %q", query, w.Code, got.Error.Message, want)
		}
	}
}

const explicitWindowParams = "compare_from=2026-02-23T00:00:00Z&compare_to=2026-02-24T06:00:00Z"

// compare_to is exclusive: an event on it is left out of the previous window.
func TestUsageSummaryComparesWithAnExplicitWindow(t *testing.T) {
	prevFrom := time.Date(2026, 2, 23, 0, 0, 0, 0, time.UTC)
	prevTo := time.Date(2026, 2, 24, 6, 0, 0, 0, time.UTC)
	reader := &filteringUsageReader{events: []usagelog.Event{
		{RequestID: "p-start", Model: "m-a", Timestamp: prevFrom, Status: 200},
		{RequestID: "p-end", Model: "m-a", Timestamp: prevTo, Status: 200},
		{RequestID: "c-1", Model: "m-a", Timestamp: time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC), Status: 200},
		{RequestID: "c-2", Model: "m-a", Timestamp: time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC), Status: 200},
	}}
	h := newCompareHarness(t, testRBAC(), reader)
	w := scopeReq(t, h, "root", http.MethodGet,
		"/usage/summary?group_by=model&from=2026-03-02T00:00:00Z&to=2026-03-02T09:00:00Z&"+explicitWindowParams, "")
	if w.Code != 200 || len(reader.queries) != 2 {
		t.Fatalf("status = %d after %d store queries, want 200 after 2: %s", w.Code, len(reader.queries), w.Body)
	}
	out := decodeCompare(t, w.Body.Bytes())
	if !out.PreviousFrom.Equal(prevFrom) || !out.PreviousTo.Equal(prevTo) {
		t.Fatalf("previous window = %v..%v, want the given one echoed", out.PreviousFrom, out.PreviousTo)
	}
	if len(out.Rows) != 1 || out.Rows[0].Requests != 2 || out.Rows[0].Previous == nil || out.Rows[0].Previous.Requests != 1 {
		t.Fatalf("rows = %+v, want m-a with 2 requests against 1 before", out.Rows)
	}
}

// Both of a comparison's queries carry the caller's scope, and a caller scoped out of everything gets the windows echoed over no rows without the store being asked.
func TestUsageSummaryCompareScopesBothWindows(t *testing.T) {
	given := [2]time.Time{time.Date(2026, 2, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 24, 6, 0, 0, 0, time.UTC)}
	for _, query := range []string{"compare=previous&since=2h", "since=2h&" + explicitWindowParams} {
		reader := &filteringUsageReader{}
		w := scopeReq(t, newCompareHarness(t, testRBAC(), reader), "alice", http.MethodGet, "/usage/summary?"+query, "")
		if w.Code != 200 || len(reader.queries) != 2 {
			t.Fatalf("%s: status = %d after %d store queries, want 200 after 2: %s", query, w.Code, len(reader.queries), w.Body)
		}
		for _, q := range reader.queries {
			if !slices.Equal(q.ScopePrincipalID, []string{"u-alice"}) || len(q.ScopeProjectID) != 0 {
				t.Errorf("%s: scope = principals %v projects %v, want [u-alice] alone", query, q.ScopePrincipalID, q.ScopeProjectID)
			}
		}

		reader = &filteringUsageReader{}
		w = scopeReq(t, newCompareHarness(t, testRBAC(), reader), "nobody", http.MethodGet, "/usage/summary?"+query, "")
		out := decodeCompare(t, w.Body.Bytes())
		if w.Code != 200 || len(reader.queries) != 0 || out.Rows == nil || len(out.Rows) != 0 || out.To.Sub(out.From) != 2*time.Hour {
			t.Fatalf("%s, scoped out: status %d after %d store queries with rows %v over %v..%v, want [] over 2h unasked", query, w.Code, len(reader.queries), out.Rows, out.From, out.To)
		}
		want := given
		if query == "compare=previous&since=2h" {
			want = [2]time.Time{out.From.Add(-2 * time.Hour), out.From}
		}
		if !out.PreviousFrom.Equal(want[0]) || !out.PreviousTo.Equal(want[1]) {
			t.Errorf("%s, scoped out: previous window = %v..%v, want %v..%v", query, out.PreviousFrom, out.PreviousTo, want[0], want[1])
		}
	}
}
