package control

import (
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
)

const explicitWindowParams = "compare_from=2026-02-23T00:00:00Z&compare_to=2026-02-24T06:00:00Z"

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
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	out := decodeCompare(t, w.Body.Bytes())
	if len(reader.queries) != 2 {
		t.Fatalf("store queries = %d, want 2", len(reader.queries))
	}
	if !out.PreviousFrom.Equal(prevFrom) || !out.PreviousTo.Equal(prevTo) {
		t.Fatalf("previous window = %v..%v, want the given one echoed", out.PreviousFrom, out.PreviousTo)
	}
	if len(out.Rows) != 1 || out.Rows[0].Requests != 2 {
		t.Fatalf("rows = %+v, want m-a with 2 requests", out.Rows)
	}
	// compare_to is exclusive: p-end on it is left out.
	if p := out.Rows[0].Previous; p == nil || p.Requests != 1 {
		t.Fatalf("previous = %+v, want 1 request", p)
	}
}

func TestUsageSummaryExplicitWindowScopesBothQueries(t *testing.T) {
	t.Run("a scoped caller's scope rides both queries", func(t *testing.T) {
		reader := &filteringUsageReader{}
		h := newCompareHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/summary?"+explicitWindowParams, "")
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

	t.Run("a caller scoped out of everything still gets the window echoed", func(t *testing.T) {
		reader := &filteringUsageReader{}
		h := newCompareHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "nobody", http.MethodGet, "/usage/summary?"+explicitWindowParams, "")
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
		if out.From.IsZero() || out.To.IsZero() {
			t.Fatalf("from/to = %v/%v, want the requested window", out.From, out.To)
		}
		if !out.PreviousFrom.Equal(time.Date(2026, 2, 23, 0, 0, 0, 0, time.UTC)) || !out.PreviousTo.Equal(time.Date(2026, 2, 24, 6, 0, 0, 0, time.UTC)) {
			t.Fatalf("previous window = %v..%v, want the given one", out.PreviousFrom, out.PreviousTo)
		}
	})
}
