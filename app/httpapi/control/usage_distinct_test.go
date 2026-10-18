package control

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
)

func TestUsageDistinctRejectsDimensionsOutsideTheAllowlist(t *testing.T) {
	for _, path := range []string{
		"/usage/summary?distinct=nonsense",
		"/usage/summary?distinct=source",
		"/usage/timeseries?interval=1h&distinct=nonsense",
		"/usage/timeseries?interval=1h&distinct=tags.env",
	} {
		t.Run(path, func(t *testing.T) {
			reader := &fakeUsageReader{}
			h := newUsageHarness(t, testRBAC(), reader)
			w := scopeReq(t, h, "root", http.MethodGet, path, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
			}
			if reader.calls != 0 {
				t.Fatalf("reader called %d times, want 0", reader.calls)
			}
		})
	}
}

func TestUsageSummaryDistinct(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: now, Status: 200, RelayKeyHash: "h-1"},
		{RequestID: "r-2", Timestamp: now, Status: 200, RelayKeyHash: "h-1"},
		{RequestID: "r-3", Timestamp: now, Status: 200, RelayKeyHash: "h-2"},
		{RequestID: "r-4", Timestamp: now, Status: 200},
	}}
	h := newUsageHarness(t, testRBAC(), reader)

	rowField := func(path string) (json.RawMessage, bool) {
		t.Helper()
		w := scopeReq(t, h, "root", http.MethodGet, path, "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		var out struct {
			Rows []map[string]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Rows) != 1 {
			t.Fatalf("rows = %d, want 1: %s", len(out.Rows), w.Body)
		}
		v, ok := out.Rows[0]["distinct"]
		return v, ok
	}

	if v, ok := rowField("/usage/summary?group_by=none"); ok {
		t.Fatalf("distinct = %s without the param, want absent", v)
	}
	if v, _ := rowField("/usage/summary?group_by=none&distinct=relay_key_hash"); string(v) != "2" {
		t.Fatalf("distinct = %s, want 2 (keyless traffic is not a key)", v)
	}
	if reader.distinct != "relay_key_hash" {
		t.Fatalf("store got distinct %q, want relay_key_hash", reader.distinct)
	}
}

func TestUsageTimeSeriesPassesDistinctToTheStore(t *testing.T) {
	reader := &fakeUsageReader{}
	h := newUsageHarness(t, testRBAC(), reader)
	w := scopeReq(t, h, "root", http.MethodGet, "/usage/timeseries?interval=1h&distinct=principal_id", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if reader.distinct != "principal_id" {
		t.Fatalf("store got distinct %q, want principal_id", reader.distinct)
	}
}
