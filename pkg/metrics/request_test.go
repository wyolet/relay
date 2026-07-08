package metrics

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func postFlightSampleCount(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := PostFlightSeconds.Write(&m); err != nil {
		t.Fatalf("write PostFlightSeconds: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// The runners record the whole detached post-flight goroutine here.
func TestRecordPostFlightTotal_Emits(t *testing.T) {
	before := postFlightSampleCount(t)
	RecordPostFlightTotal(3 * time.Millisecond)
	if after := postFlightSampleCount(t); after != before+1 {
		t.Fatalf("post_flight_seconds sample count = %d, want %d after RecordPostFlightTotal", after, before+1)
	}
}

// RecordPostFlight is retained for the finalize-observer wiring but must no
// longer feed the histogram — otherwise every success double-counts (observer
// fan-out sample + runner whole-goroutine sample).
func TestRecordPostFlight_IsNoOp(t *testing.T) {
	before := postFlightSampleCount(t)
	RecordPostFlight(3 * time.Millisecond)
	if after := postFlightSampleCount(t); after != before {
		t.Fatalf("RecordPostFlight fed the histogram (count %d → %d); it must be a no-op to avoid double-counting", before, after)
	}
}
