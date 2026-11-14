package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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

// The total is the whole filtered set whatever the page and cursor, counted one past the cap so the cap itself is not reported as capped; without count=true nothing is counted, and a caller scoped out of every row never reaches the store.
func TestLogsListCount(t *testing.T) {
	cursor := encodeCursor(time.Now().Add(-time.Minute), "r-00000")
	alices := append(recentEvents(2, "u-alice"), recentEvents(5, "u-bob")...)
	for _, tc := range []struct {
		name, who, query string
		events           []usagelog.Event
		total, capped    string
		calls            int
	}{
		{"count off", "root", "", recentEvents(3, ""), "", "", 1},
		{"count on, a later page", "root", "?count=true&limit=1&cursor=" + cursor, recentEvents(3, ""), "3", "false", 2},
		{"exactly the cap", "root", "?count=true&limit=1", recentEvents(logsTotalCap, ""), "10000", "false", 2},
		{"more than the cap", "root", "?count=true&limit=1", recentEvents(logsTotalCap+2, ""), "10000", "true", 2},
		{"the caller's scope", "alice", "?count=true", alices, "2", "false", 2},
		{"a caller scoped out", "nobody", "?count=true", recentEvents(3, ""), "0", "false", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeUsageReader{events: tc.events}
			var out map[string]json.RawMessage
			wantJSON(t, scopeReq(t, newUsageHarness(t, testRBAC(), reader), tc.who, http.MethodGet, "/logs"+tc.query, ""), http.StatusOK, &out)
			// The raw fields tell an absent total from a zero one.
			if total, capped := string(out["total"]), string(out["total_capped"]); total != tc.total || capped != tc.capped {
				t.Errorf("total=%q total_capped=%q, want %q/%q", total, capped, tc.total, tc.capped)
			}
			if reader.calls != tc.calls {
				t.Errorf("reader called %d times, want %d", reader.calls, tc.calls)
			}
			if tc.calls == 2 && reader.countLimit != logsTotalCap+1 {
				t.Errorf("count limit = %d, want %d", reader.countLimit, logsTotalCap+1)
			}
			if tc.who == "alice" && !slices.Equal(reader.lastCount.ScopePrincipalID, []string{"u-alice"}) {
				t.Errorf("count ScopePrincipalID = %v, want [u-alice]", reader.lastCount.ScopePrincipalID)
			}
		})
	}
}
