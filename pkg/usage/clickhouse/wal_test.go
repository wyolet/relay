package clickhouse

import (
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/usage"
)

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

// The queue's own behaviour is pinned in pkg/wal; this pins how the sink wires it: usage events survive the segment file unchanged and the lag gauge is reported under kind "usage".
func TestSegmentQueueCarriesUsageEvents(t *testing.T) {
	dir := t.TempDir()
	events := sampleEvents()
	lag := func() float64 { return testutil.ToFloat64(metrics.FlushLag.WithLabelValues("usage")) }

	down, err := newSegmentQueue(dir, len(events), time.Hour, 1024, testLogger, func([]usage.Event) error { return errors.New("sink down") })
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	for _, ev := range events {
		if err := down.Write(ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	down.FlushPending()
	if got := lag(); got <= 0 {
		t.Fatalf("usage flush lag = %v while the sink refuses the segment, want > 0", got)
	}
	down.StopBackground()

	var got []usage.Event
	up, err := newSegmentQueue(dir, len(events), time.Hour, 1024, testLogger, func(evs []usage.Event) error {
		got = append(got, evs...)
		return nil
	})
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	up.Recover()
	if err := up.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// JSON drops the zone name and turns empty maps into absent ones; the instants and contents must match.
	if diff := cmp.Diff(events, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("events read back from the segment (-written +flushed):\n%s", diff)
	}
	if got := lag(); got != 0 {
		t.Fatalf("usage flush lag = %v after the drain, want 0", got)
	}
}
