package keypool

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestRecord_WireForm(t *testing.T) {
	ms := func(n int64) time.Time { return time.UnixMilli(n).UTC() }
	for _, tc := range []struct {
		name string
		rec  CircuitRecord
		wire string
	}{
		{"closed default", CircuitRecord{}, `{"state":0,"backoff_step":0}`},
		{"every field", CircuitRecord{
			State: CircuitHalfOpen, OpenUntil: ms(1500), BackoffStep: 3, LastTransition: ms(1000), Indefinite: true,
			Reason: ReasonUpstreamForbidden, Failures: 2, WindowStart: ms(900), ProbeUntil: ms(2500),
		}, `{"state":2,"open_until_ms":1500,"backoff_step":3,"last_transition_ms":1000,"indefinite":true,"reason":"upstream_forbidden","failures":2,"window_start_ms":900,"probe_until_ms":2500}`},
	} {
		b, err := encodeRecord(tc.rec)
		if err != nil || string(b) != tc.wire {
			t.Errorf("%s: encodeRecord = %s, %v; want %s", tc.name, b, err, tc.wire)
		}
		got, err := decodeRecord([]byte(tc.wire))
		if d := cmp.Diff(tc.rec, got); d != "" || err != nil {
			t.Errorf("%s: decodeRecord: %v (-want +got):\n%s", tc.name, err, d)
		}
	}
}

func TestDecodeRecord_LegacyAndInvalid(t *testing.T) {
	// The form before times were milliseconds and before reasons: its RFC 3339 times are not read.
	legacy := `{"state":1,"open_until":"2026-01-01T00:00:30Z","backoff_step":2,"last_transition":"2026-01-01T00:00:00Z","indefinite":false}`
	got, err := decodeRecord([]byte(legacy))
	if d := cmp.Diff(CircuitRecord{State: CircuitOpen, BackoffStep: 2}, got); d != "" || err != nil {
		t.Errorf("legacy record: %v (-want +got):\n%s", err, d)
	}
	for _, raw := range []string{``, `not json`, `{"state":"open"}`, `[]`} {
		if got, err := decodeRecord([]byte(raw)); err == nil || got != (CircuitRecord{}) {
			t.Errorf("decodeRecord(%q) = %+v, %v; want an error", raw, got, err)
		}
	}
}

func TestCircuitState_String(t *testing.T) {
	for state, want := range map[CircuitState]string{
		CircuitClosed: "closed", CircuitOpen: "open", CircuitHalfOpen: "half_open", 3: "unknown", -1: "unknown",
	} {
		if got := state.String(); got != want {
			t.Errorf("CircuitState(%d).String() = %q, want %q", state, got, want)
		}
	}
}
