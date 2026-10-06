package usagelog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
)

// An upstream status of 400 or above always carries the upstream-error kind; a kind the runner already set is kept, and a status below 400 gets none.
func TestUsageHook_ErrorStatusCarriesUpstreamErrorKind(t *testing.T) {
	cases := []struct {
		name     string
		ev       lifecycle.PostFlightEvent
		wantKind string
		wantMsg  string
	}{
		{"success", lifecycle.PostFlightEvent{Status: 200}, "", ""},
		{"redirect", lifecycle.PostFlightEvent{Status: 304}, "", ""},
		{"passed-through 400", lifecycle.PostFlightEvent{Status: 400, ResponseBody: []byte(`{"error":{"message":"prompt: secret text"}}`)}, usage.ErrorKindUpstream, "upstream returned 400"},
		{"passed-through 404", lifecycle.PostFlightEvent{Status: 404}, usage.ErrorKindUpstream, "upstream returned 404"},
		{"passed-through 503", lifecycle.PostFlightEvent{Status: 503}, usage.ErrorKindUpstream, "upstream returned 503"},
		{"runner-classified failure", lifecycle.PostFlightEvent{Status: 429, ErrorKind: "upstream_error", ErrorMessage: "upstream returned 429: slow down"}, usage.ErrorKindUpstream, "upstream returned 429: slow down"},
		{"rejected before upstream", lifecycle.PostFlightEvent{ErrorKind: "rate_limited", ErrorMessage: "quota"}, "rate_limited", "quota"},
	}
	hook := NewUsageHook(nil, "")
	for _, tc := range cases {
		lc := lifecycle.NewContext("req", "pipeline", time.Now())
		v, err := hook.Fill(lc, &tc.ev)
		if err != nil {
			t.Fatalf("%s: Fill: %v", tc.name, err)
		}
		got := v.(*Event)
		if got.Status != tc.ev.Status || got.ErrorKind != tc.wantKind || got.ErrorMessage != tc.wantMsg {
			t.Errorf("%s: status=%d kind=%q message=%q, want status=%d kind=%q message=%q",
				tc.name, got.Status, got.ErrorKind, got.ErrorMessage, tc.ev.Status, tc.wantKind, tc.wantMsg)
		}
		if tc.ev.Status != 0 && got.LogOnly() {
			t.Errorf("%s: an event with an upstream status must stay in aggregates", tc.name)
		}
	}
}

// A streamed response records the status the runner stamped, not a fixed 200.
func TestStreamUsageObserver_RecordsTheStampedStatus(t *testing.T) {
	cases := []struct {
		stamped    int
		wantStatus int
		wantKind   string
	}{
		{0, 200, ""},
		{200, 200, ""},
		{400, 400, usage.ErrorKindUpstream},
		{529, 529, usage.ErrorKindUpstream},
	}
	for _, tc := range cases {
		lc := lifecycle.NewContext("req", "pipeline", time.Now())
		lc.ResponseStatus = tc.stamped
		obs := NewStreamUsageFactory(nil, "").NewObserver(lc)
		obs.Observe([]byte(`{"error":{"message":"bad request"}}`))
		v, err := obs.Result()
		if err != nil {
			t.Fatalf("stamped %d: Result: %v", tc.stamped, err)
		}
		got := v.(*Event)
		if got.Status != tc.wantStatus || got.ErrorKind != tc.wantKind {
			t.Errorf("stamped %d: status=%d kind=%q, want status=%d kind=%q", tc.stamped, got.Status, got.ErrorKind, tc.wantStatus, tc.wantKind)
		}
	}
}
