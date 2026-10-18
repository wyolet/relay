package usage

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAttachPreviousJoinsByGroup(t *testing.T) {
	rows := []SummaryRow{
		{Group: map[string]string{"model": "a"}, Requests: 5},
		{Group: map[string]string{"model": "b"}, Requests: 3},
	}
	prev := []SummaryRow{
		{Group: map[string]string{"model": "c"}, Requests: 9},
		{Group: map[string]string{"model": "a"}, Requests: 2, ErrorCount: 1, Errors5xx: 1, CostNanos: 7, Unpriced: 1,
			Tokens: map[string]int64{"input": 4}, DurationMs: DurationStats{Avg: 10}, TTFTMs: &DurationStats{Avg: 3}},
	}
	got := attachPrevious(rows, prev)
	if len(got) != 2 {
		t.Fatalf("rows = %d, want the 2 current rows only", len(got))
	}
	a := got[0].Previous
	if a == nil || a.Requests != 2 || a.ErrorCount != 1 || a.Errors5xx != 1 || a.CostNanos != 7 || a.Unpriced != 1 ||
		a.Tokens["input"] != 4 || a.DurationMs.Avg != 10 || a.TTFTMs == nil || a.TTFTMs.Avg != 3 {
		t.Fatalf("model a previous = %+v", a)
	}
	if got[1].Previous != nil {
		t.Fatalf("model b previous = %+v, want nil (no previous traffic)", got[1].Previous)
	}
}

func TestAttachPreviousMatchesTheEmptyGroupOfNone(t *testing.T) {
	got := attachPrevious(
		[]SummaryRow{{Group: map[string]string{}, Requests: 5}},
		[]SummaryRow{{Group: map[string]string{}, Requests: 4}},
	)
	if got[0].Previous == nil || got[0].Previous.Requests != 4 {
		t.Fatalf("previous = %+v, want requests 4", got[0].Previous)
	}
}

func TestGroupIdentityKeepsDistinctGroupsApart(t *testing.T) {
	if groupIdentity(map[string]string{"tags.a": "b"}) == groupIdentity(map[string]string{"tags.ab": ""}) {
		t.Fatal("distinct groups share an identity")
	}
	if groupIdentity(map[string]string{}) != groupIdentity(nil) {
		t.Fatal("empty and nil groups differ")
	}
}

func TestSummaryRowOmitsAnAbsentPrevious(t *testing.T) {
	b, err := json.Marshal(SummaryResult{Rows: []SummaryRow{{}}})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"previous_from", "previous_to"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s present without a comparison: %s", k, b)
		}
	}
	if _, ok := out["rows"].([]any)[0].(map[string]any)["previous"]; ok {
		t.Errorf("previous present without a comparison: %s", b)
	}
}

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	from, to := now.Add(-3*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name             string
		q                EventQuery
		wantFrom, wantTo time.Time
		wantErr          bool
	}{
		{name: "since ends now", q: EventQuery{Since: time.Hour}, wantFrom: now.Add(-time.Hour), wantTo: now},
		{name: "from overrides since", q: EventQuery{Since: time.Hour, From: from}, wantFrom: from, wantTo: now},
		{name: "absolute bounds", q: EventQuery{From: from, To: to}, wantFrom: from, wantTo: to},
		// Backends resolve since against now even when to is set, so the window is checked as they apply it.
		{name: "since with an earlier to", q: EventQuery{Since: time.Hour, To: from}, wantErr: true},
		{name: "no lower bound", q: EventQuery{To: to}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotFrom, gotTo, err := ResolveWindow(tc.q, now)
			if tc.wantErr {
				if !errors.Is(err, ErrNoPreviousWindow) {
					t.Fatalf("err = %v, want ErrNoPreviousWindow", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !gotFrom.Equal(tc.wantFrom) || !gotTo.Equal(tc.wantTo) {
				t.Fatalf("window = %v..%v, want %v..%v", gotFrom, gotTo, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestPreviousWindowEndsWhereTheWindowStarts(t *testing.T) {
	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	prevFrom, prevTo := PreviousWindow(from, from.Add(90*time.Minute))
	if !prevTo.Equal(from) || !prevFrom.Equal(from.Add(-90*time.Minute)) {
		t.Fatalf("previous = %v..%v", prevFrom, prevTo)
	}
}

// queryRecorder is a Reader that records every Summary query and answers from rows keyed by the query's lower bound.
type queryRecorder struct {
	mu      sync.Mutex
	queries []SummaryQuery
	rows    map[time.Time][]SummaryRow
	err     error
}

func (r *queryRecorder) Events(context.Context, EventQuery) ([]Event, error) { return nil, nil }

func (r *queryRecorder) Count(context.Context, EventQuery, int) (int64, error) { return 0, nil }

func (r *queryRecorder) TimeSeries(context.Context, TimeSeriesQuery) (TimeSeriesResult, error) {
	return TimeSeriesResult{}, nil
}

func (r *queryRecorder) Facet(context.Context, FacetQuery) ([]FacetValue, error) { return nil, nil }

func (r *queryRecorder) Summary(_ context.Context, q SummaryQuery) (SummaryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
	return SummaryResult{Rows: r.rows[q.From]}, r.err
}

func TestSummaryWithPrecedingWindowQueriesBothWindowsWithTheSameFilters(t *testing.T) {
	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	prevFrom := from.Add(-time.Hour)
	r := &queryRecorder{rows: map[time.Time][]SummaryRow{
		from:     {{Group: map[string]string{"tags.env": "prod"}, Requests: 3}},
		prevFrom: {{Group: map[string]string{"tags.env": "prod"}, Requests: 1}},
	}}
	q := SummaryQuery{
		EventQuery: EventQuery{From: from, To: to, Model: []string{"m"}, ScopePrincipalID: []string{"u-1"}},
		GroupBy:    "tags.env",
	}
	res, err := SummaryWithPrecedingWindow(t.Context(), r, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(r.queries))
	}
	for _, got := range r.queries {
		if got.GroupBy != "tags.env" || len(got.Model) != 1 || len(got.ScopePrincipalID) != 1 || got.Since != 0 {
			t.Errorf("query = %+v, want the caller's filters, group and scope", got)
		}
		switch {
		case got.From.Equal(from):
			if !got.To.Equal(to) || !got.Before.IsZero() {
				t.Errorf("current window query = %v..%v before %v", got.From, got.To, got.Before)
			}
		case got.From.Equal(prevFrom):
			if !got.To.IsZero() || !got.Before.Equal(from) {
				t.Errorf("previous window query = %v..%v before %v, want an exclusive end at %v", got.From, got.To, got.Before, from)
			}
		default:
			t.Errorf("unexpected window starting %v", got.From)
		}
	}
	if len(res.Rows) != 1 || res.Rows[0].Previous == nil || res.Rows[0].Previous.Requests != 1 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	if !res.PreviousFrom.Equal(prevFrom) || !res.PreviousTo.Equal(from) {
		t.Fatalf("previous window = %v..%v", res.PreviousFrom, res.PreviousTo)
	}
}

func TestSummaryWithPreviousQueriesTheGivenWindow(t *testing.T) {
	prevFrom := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	prevTo := prevFrom.Add(30 * time.Minute)
	r := &queryRecorder{}
	q := SummaryQuery{EventQuery: EventQuery{Since: 2 * time.Hour, Model: []string{"m"}, ScopeProjectID: []string{"p-1"}}, GroupBy: "model"}
	res, err := SummaryWithPrevious(t.Context(), r, q, prevFrom, prevTo)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(r.queries))
	}
	for _, got := range r.queries {
		if got.GroupBy != "model" || len(got.Model) != 1 || len(got.ScopeProjectID) != 1 {
			t.Errorf("query = %+v, want the caller's filters, group and scope", got)
		}
		if got.From.IsZero() {
			if got.Since != 2*time.Hour || !got.Before.IsZero() {
				t.Errorf("current window query = %+v, want q unchanged", got)
			}
			continue
		}
		if !got.From.Equal(prevFrom) || !got.Before.Equal(prevTo) || !got.To.IsZero() || got.Since != 0 {
			t.Errorf("previous window query = %v..%v before %v since %v, want [%v, %v)", got.From, got.To, got.Before, got.Since, prevFrom, prevTo)
		}
	}
	if !res.PreviousFrom.Equal(prevFrom) || !res.PreviousTo.Equal(prevTo) {
		t.Fatalf("previous window = %v..%v, want the given one", res.PreviousFrom, res.PreviousTo)
	}
}

func TestSummaryWithPrecedingWindowRejectsAWindowWithoutAStart(t *testing.T) {
	r := &queryRecorder{}
	_, err := SummaryWithPrecedingWindow(t.Context(), r, SummaryQuery{})
	if !errors.Is(err, ErrNoPreviousWindow) {
		t.Fatalf("err = %v, want ErrNoPreviousWindow", err)
	}
	if len(r.queries) != 0 {
		t.Fatalf("queries = %d, want none", len(r.queries))
	}
}

func TestSummaryWithPreviousReturnsAReaderError(t *testing.T) {
	boom := errors.New("boom")
	r := &queryRecorder{err: boom}
	_, err := SummaryWithPrecedingWindow(t.Context(), r, SummaryQuery{EventQuery: EventQuery{Since: time.Hour}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's", err)
	}
}
