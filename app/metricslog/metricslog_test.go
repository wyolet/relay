package metricslog

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
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

// PreFlight raises the inflight gauge and arms the Metadata note; Collect
// lowers it only when the note is present — a request that never ran
// pre-flight (batch) must not drive the gauge negative.
func TestHook_InflightPairing(t *testing.T) {
	h := New()
	gauge := metrics.InflightRequests.WithLabelValues("pipeline")
	base := testutil.ToFloat64(gauge)

	lc := lifecycle.NewContext("req-3", "pipeline", time.Now())
	if err := h.PreFlight(context.Background(), lc, &lifecycle.PreFlightEvent{}); err != nil {
		t.Fatalf("PreFlight: %v", err)
	}
	if got := testutil.ToFloat64(gauge); got != base+1 {
		t.Fatalf("gauge after PreFlight = %v, want %v", got, base+1)
	}
	if _, ok := lc.Metadata[inflightKey]; !ok {
		t.Fatal("PreFlight did not arm the inflight note")
	}

	h.Collect(lc)
	if got := testutil.ToFloat64(gauge); got != base {
		t.Fatalf("gauge after Collect = %v, want %v", got, base)
	}

	// No pre-flight ran for this one: Collect must not decrement.
	h.Collect(lifecycle.NewContext("req-4", "pipeline", time.Now()))
	if got := testutil.ToFloat64(gauge); got != base {
		t.Fatalf("gauge after unpaired Collect = %v, want %v", got, base)
	}
}

// Admission is observed only when the request reached upstream
// (Timing.Upstream.Start stamped).
func TestHook_Fill_AdmissionGuard(t *testing.T) {
	h := New()
	base := testutil.CollectAndCount(metrics.AdmissionSeconds)

	lcNone := lifecycle.NewContext("req-5", "adm-none", time.Now())
	lcNone.Timing.End = 2 * time.Millisecond
	if _, err := h.Fill(lcNone, &lifecycle.PostFlightEvent{Status: 401}); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := testutil.CollectAndCount(metrics.AdmissionSeconds); got != base {
		t.Fatalf("admission observed for a request that never reached upstream (series %d, want %d)", got, base)
	}

	lcYes := lifecycle.NewContext("req-6", "adm-yes", time.Now())
	lcYes.Timing.End = 100 * time.Millisecond
	lcYes.Timing.Upstream.Start = 3 * time.Millisecond
	if _, err := h.Fill(lcYes, &lifecycle.PostFlightEvent{Status: 200}); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := testutil.CollectAndCount(metrics.AdmissionSeconds); got != base+1 {
		t.Fatalf("admission not observed (series %d, want %d)", got, base+1)
	}
}
