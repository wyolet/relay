package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
)

func recentEvents(n int, principalID string) []usagelog.Event {
	now := time.Now().UTC()
	events := make([]usagelog.Event, n)
	for i := range events {
		events[i] = usagelog.Event{RequestID: fmt.Sprintf("r-%05d", i), Timestamp: now, PrincipalID: principalID}
	}
	return events
}

// logsTotals decodes the raw total fields so a test can tell an absent field from a zero one.
func logsTotals(t *testing.T, body []byte) (total, capped json.RawMessage) {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out["total"], out["total_capped"]
}

func TestLogsListCount(t *testing.T) {
	t.Run("count off adds no total and never counts", func(t *testing.T) {
		reader := &fakeUsageReader{events: recentEvents(3, "")}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, "/logs", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if total, capped := logsTotals(t, w.Body.Bytes()); total != nil || capped != nil {
			t.Fatalf("total=%s total_capped=%s, want both absent", total, capped)
		}
		if reader.calls != 1 {
			t.Fatalf("reader called %d times, want 1 (no count)", reader.calls)
		}
	})

	t.Run("count on reports the whole filtered set regardless of page and cursor", func(t *testing.T) {
		reader := &fakeUsageReader{events: recentEvents(3, "")}
		h := newUsageHarness(t, testRBAC(), reader)
		cursor := encodeCursor(time.Now().Add(-time.Minute), "r-00000")
		w := scopeReq(t, h, "root", http.MethodGet, "/logs?count=true&limit=1&cursor="+cursor, "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if total, capped := logsTotals(t, w.Body.Bytes()); string(total) != "3" || string(capped) != "false" {
			t.Fatalf("total=%s total_capped=%s, want 3/false", total, capped)
		}
		if reader.countLimit != logsTotalCap+1 {
			t.Fatalf("count limit = %d, want %d", reader.countLimit, logsTotalCap+1)
		}
	})

	t.Run("more than the cap reports the cap and total_capped", func(t *testing.T) {
		reader := &fakeUsageReader{events: recentEvents(logsTotalCap+2, "")}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, "/logs?count=true&limit=1", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if total, capped := logsTotals(t, w.Body.Bytes()); string(total) != "10000" || string(capped) != "true" {
			t.Fatalf("total=%s total_capped=%s, want 10000/true", total, capped)
		}
	})

	t.Run("exactly the cap is not capped", func(t *testing.T) {
		reader := &fakeUsageReader{events: recentEvents(logsTotalCap, "")}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, "/logs?count=true&limit=1", "")
		if total, capped := logsTotals(t, w.Body.Bytes()); string(total) != "10000" || string(capped) != "false" {
			t.Fatalf("total=%s total_capped=%s, want 10000/false", total, capped)
		}
	})

	t.Run("the count carries the caller's scope", func(t *testing.T) {
		reader := &fakeUsageReader{events: append(recentEvents(2, "u-alice"), recentEvents(5, "u-bob")...)}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/logs?count=true", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if got := reader.lastCount.ScopePrincipalID; len(got) != 1 || got[0] != "u-alice" {
			t.Fatalf("count ScopePrincipalID = %v, want [u-alice]", got)
		}
		if total, _ := logsTotals(t, w.Body.Bytes()); string(total) != "2" {
			t.Fatalf("total = %s, want 2", total)
		}
	})

	t.Run("a caller scoped out gets a zero total without reaching the store", func(t *testing.T) {
		reader := &fakeUsageReader{events: recentEvents(3, "")}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "nobody", http.MethodGet, "/logs?count=true", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if total, capped := logsTotals(t, w.Body.Bytes()); string(total) != "0" || string(capped) != "false" {
			t.Fatalf("total=%s total_capped=%s, want 0/false", total, capped)
		}
		if reader.calls != 0 {
			t.Fatalf("reader called %d times, want 0", reader.calls)
		}
	})
}
