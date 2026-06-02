package metricslog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// The hook's only logic is the overhead split: observe it only when
// upstream was reached, and compute it as total − upstream-leg.
func TestHook_Fill_OverheadGuard(t *testing.T) {
	h := New()

	// Upstream reached: Start + ResponseEnd both stamped.
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
	lc.Timing.End = 100 * time.Millisecond
	lc.Timing.Upstream.Start = 5 * time.Millisecond
	lc.Timing.Upstream.ResponseEnd = 95 * time.Millisecond
	if got := upstreamDuration(lc); got != 90*time.Millisecond {
		t.Fatalf("upstreamDuration = %v, want 90ms", got)
	}
	if _, err := h.Fill(lc, &lifecycle.PostFlightEvent{Status: 200}); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	// Upstream never reached (e.g. auth rejected pre-flight): no overhead.
	lc2 := lifecycle.NewContext("req-2", "pipeline", time.Now())
	lc2.Timing.End = 2 * time.Millisecond
	if got := upstreamDuration(lc2); got != 0 {
		t.Fatalf("upstreamDuration with no upstream = %v, want 0", got)
	}
	if _, err := h.Fill(lc2, &lifecycle.PostFlightEvent{Status: 401}); err != nil {
		t.Fatalf("Fill: %v", err)
	}
}
