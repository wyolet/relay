package control

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

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

func (f *filteringUsageReader) Summary(_ context.Context, q usagelog.SummaryQuery) (usagelog.SummaryResult, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return usage.Summarize(usage.FilterEvents(f.events, q.EventQuery), q.GroupBy)
}

func (f *filteringUsageReader) TimeSeries(context.Context, usagelog.TimeSeriesQuery) (usagelog.TimeSeriesResult, error) {
	return usagelog.TimeSeriesResult{}, nil
}

func newCompareHarness(t *testing.T, authzr authz.Authorizer, reader usagelog.Reader) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usage-compare-test", "0"))
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
	for _, path := range []string{
		"/usage/summary?compare=yesterday",
		"/usage/summary?compare=previous&since=0s",
	} {
		reader := &filteringUsageReader{}
		h := newCompareHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, path, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", path, w.Code, w.Body)
		}
	}
}

func TestUsageSummaryCompareScopesBothWindows(t *testing.T) {
	t.Run("a scoped caller's scope rides both queries", func(t *testing.T) {
		reader := &filteringUsageReader{}
		h := newCompareHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/summary?compare=previous&since=2h", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if len(reader.queries) != 2 {
			t.Fatalf("store queries = %d, want 2", len(reader.queries))
		}
		for _, q := range reader.queries {
			if len(q.ScopePrincipalID) != 1 || q.ScopePrincipalID[0] != "u-alice" {
				t.Errorf("ScopePrincipalID = %v, want [u-alice]", q.ScopePrincipalID)
			}
		}
	})

	t.Run("a caller scoped out of everything gets an empty comparison", func(t *testing.T) {
		reader := &filteringUsageReader{}
		h := newCompareHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "nobody", http.MethodGet, "/usage/summary?compare=previous&since=2h", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if len(reader.queries) != 0 {
			t.Fatalf("store queries = %d, want 0", len(reader.queries))
		}
		out := decodeCompare(t, w.Body.Bytes())
		if out.Rows == nil || len(out.Rows) != 0 {
			t.Fatalf("rows = %v, want []: %s", out.Rows, w.Body)
		}
		if span := out.To.Sub(out.From); span != 2*time.Hour {
			t.Fatalf("window = %v, want 2h", span)
		}
		if !out.PreviousTo.Equal(out.From) || out.PreviousTo.Sub(out.PreviousFrom) != 2*time.Hour {
			t.Fatalf("previous window = %v..%v, want the 2h before %v", out.PreviousFrom, out.PreviousTo, out.From)
		}
	})
}
