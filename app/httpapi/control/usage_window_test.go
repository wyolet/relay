package control

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A summary over a window with no events still has to report the window it
// covered — a zero from/to gives a chart nothing to draw an empty range on.
func TestUsageSummaryReportsTheRequestedWindowWhenEmpty(t *testing.T) {
	h := newUsageHarness(t, testRBAC(), &fakeUsageReader{})
	w := scopeReq(t, h, "root", http.MethodGet, "/usage/summary?since=2h", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var out struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.From.IsZero() || out.To.IsZero() {
		t.Fatalf("from/to = %v/%v, want the requested window", out.From, out.To)
	}
	if span := out.To.Sub(out.From); span < 119*time.Minute || span > 121*time.Minute {
		t.Fatalf("window = %v, want the requested 2h", span)
	}
}
