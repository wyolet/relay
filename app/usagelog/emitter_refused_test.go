package usagelog

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

// A flood of refused requests' events while the sink is stalled must drop only
// refusal rows: every served request's row still reaches the sink.
func TestEmitter_RefusalsNeverDisplaceServedEvents(t *testing.T) {
	const (
		servedN    = 200
		flooders   = 8
		perFlooder = 1000
		refusedCap = 16
	)
	lostUsage := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("usage"))
	lostRefused := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("usage_refused"))

	sink := newBatchSink()
	e := NewEmitter(EmitterOptions{QueueSize: servedN, RefusedQueueSize: refusedCap, Logger: testLogger()}, sink)
	e.Emit(served("parked"))
	<-sink.entered

	var wg sync.WaitGroup
	for f := range flooders {
		wg.Go(func() {
			for i := range perFlooder {
				e.Emit(Event{RequestID: fmt.Sprintf("refused-%d-%d", f, i), Status: 429, ErrorKind: "overloaded"})
			}
		})
	}
	want := []string{"parked"}
	for i := range servedN {
		id := fmt.Sprintf("served-%d", i)
		want = append(want, id)
		e.Emit(served(id))
	}
	wg.Wait()

	if e.Dropped() != 0 {
		t.Fatalf("served events dropped = %d, want 0", e.Dropped())
	}
	if got, wantDrops := e.DroppedRefused(), uint64(flooders*perFlooder-refusedCap); got != wantDrops {
		t.Fatalf("refused events dropped = %d, want %d", got, wantDrops)
	}
	if d := e.RefusedQueueDepth(); d != refusedCap {
		t.Fatalf("refused queue depth = %d, want %d (full)", d, refusedCap)
	}
	if got := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("usage")) - lostUsage; got != 0 {
		t.Fatalf("records lost {kind=usage} rose by %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("usage_refused")) - lostRefused; got != float64(e.DroppedRefused()) {
		t.Fatalf("records lost {kind=usage_refused} rose by %v, want %d", got, e.DroppedRefused())
	}

	close(sink.release)
	e.Close()
	_, written := sink.result()
	var servedWritten []string
	refusedWritten := 0
	for _, id := range written {
		if id == "parked" || len(id) > 7 && id[:7] == "served-" {
			servedWritten = append(servedWritten, id)
		} else {
			refusedWritten++
		}
	}
	if !slices.Equal(servedWritten, want) {
		t.Fatalf("served rows written = %d, want all %d in order", len(servedWritten), len(want))
	}
	if refusedWritten != refusedCap {
		t.Fatalf("refused rows written = %d, want the %d that fit", refusedWritten, refusedCap)
	}
}

// The drain empties the served queue before the refused one, so a slow sink
// spends its time on billable rows first.
func TestEmitter_DrainsServedBeforeRefused(t *testing.T) {
	sink := newBatchSink()
	e := NewEmitter(EmitterOptions{QueueSize: 100, RefusedQueueSize: 100, Logger: testLogger()}, sink)
	e.Emit(served("parked"))
	<-sink.entered
	for i := range 50 {
		e.Emit(Event{RequestID: fmt.Sprintf("refused-%d", i)})
		e.Emit(served(fmt.Sprintf("served-%d", i)))
	}
	close(sink.release)
	e.Close()

	_, written := sink.result()
	if len(written) != 101 {
		t.Fatalf("written = %d, want 101", len(written))
	}
	// The first batch after the parked one may start with the one refused
	// event the drain's select picked; every served row still precedes the
	// remaining refused rows.
	lastServed, firstRefused := -1, len(written)
	for i, id := range written[1:] {
		if id[:7] == "served-" {
			lastServed = i
		} else if i > 0 && i < firstRefused {
			firstRefused = i
		}
	}
	if lastServed > firstRefused {
		t.Fatalf("a served row was written after refused rows: %v", written)
	}
}
