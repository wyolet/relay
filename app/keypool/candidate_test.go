package keypool

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
)

// A success changes a record only for a half-open probe or a backoff to reset; a clean closed record, or none, is left alone.
func TestCandidate_SuccessNeedsWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Candidate
		want bool
	}{
		{"no record", Candidate{Record: CircuitRecord{State: CircuitClosed}}, false},
		{"clean closed", Candidate{Record: CircuitRecord{State: CircuitClosed}, Stored: true}, false},
		{"closed with backoff", Candidate{Record: CircuitRecord{State: CircuitClosed, BackoffStep: 2}, Stored: true}, true},
		{"half-open probe", Candidate{Record: CircuitRecord{State: CircuitHalfOpen}, Stored: true}, true},
	} {
		if got := tc.c.SuccessNeedsWrite(); got != tc.want {
			t.Errorf("%s: SuccessNeedsWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// PickCandidate reports the record a key was picked on: none for a key that never failed or the anonymous key, the promoted half-open record for a key whose cooldown ran out.
func TestPickCandidate_ReportsTheRecord(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	keys := []*hostkey.HostKey{key("a", "hA")}

	c, err := sel.PickCandidate(ctx, "cand", KeySelectionPrioritized, keys, nil)
	if err != nil || c.Key != keys[0] || c.Stored {
		t.Fatalf("fresh key = %+v, %v; want no stored record", c, err)
	}
	anon := key("anon", hostkey.AnonIDPrefix+"host")
	if c, _ := sel.PickCandidate(ctx, "cand", KeySelectionPrioritized, []*hostkey.HostKey{anon}, nil); c.Stored || c.SuccessNeedsWrite() {
		t.Fatalf("anonymous key = %+v, want no record and no success write", c)
	}

	sel.RecordFailure(ctx, "hA", FailureRateLimitLong, 0)
	c, err = sel.PickCandidate(ctx, "cand", KeySelectionRoundRobin, keys, nil)
	if err != nil || !c.Stored || c.Record.State != CircuitHalfOpen || !c.SuccessNeedsWrite() {
		t.Fatalf("probe = %+v, %v; want the half-open record", c, err)
	}
}
