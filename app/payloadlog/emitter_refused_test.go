package payloadlog

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// recordingSink parks its first Write until release closes, then records
// every request id it is handed.
type recordingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	written []string
}

func newRecordingSink() *recordingSink {
	return &recordingSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *recordingSink) Write(r Record) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, r.RequestID)
	return nil
}

func (s *recordingSink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.written)
}

// A flood of refused requests' Records while the sink is stalled must drop
// only refusal Records: every served request's Record still reaches the sink.
func TestEmitter_RefusalsNeverDisplaceServedRecords(t *testing.T) {
	const (
		servedN    = 200
		flooders   = 8
		perFlooder = 1000
		refusedCap = 8
	)
	lostServed := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("payload"))
	lostRefused := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("payload_refused"))

	sink := newRecordingSink()
	refusedBytes := refusedCap * recordBytes(Record{RequestID: "refused-0-0000", RequestBody: []byte(`{}`)})
	e := NewEmitter(EmitterOptions{QueueBytes: 1 << 20, RefusedQueueBytes: refusedBytes, Logger: testLogger()}, sink)
	e.Emit(Record{RequestID: "parked"})
	<-sink.entered

	var wg sync.WaitGroup
	for f := range flooders {
		wg.Go(func() {
			for i := range perFlooder {
				e.EmitRefused(Record{RequestID: fmt.Sprintf("refused-%d-%04d", f, i), RequestBody: []byte(`{}`)})
			}
		})
	}
	want := []string{"parked"}
	for i := range servedN {
		id := fmt.Sprintf("served-%d", i)
		want = append(want, id)
		e.Emit(Record{RequestID: id})
	}
	wg.Wait()

	if e.Dropped() != 0 {
		t.Fatalf("served records dropped = %d, want 0", e.Dropped())
	}
	if got, wantDrops := e.DroppedRefused(), uint64(flooders*perFlooder-refusedCap); got != wantDrops {
		t.Fatalf("refused records dropped = %d, want %d", got, wantDrops)
	}
	if got := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("payload")) - lostServed; got != 0 {
		t.Fatalf("records lost {kind=payload} rose by %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.RecordsLost.WithLabelValues("payload_refused")) - lostRefused; got != float64(e.DroppedRefused()) {
		t.Fatalf("records lost {kind=payload_refused} rose by %v, want %d", got, e.DroppedRefused())
	}

	close(sink.release)
	e.Close()
	written := sink.ids()
	// Served Records drain strictly first, then the refused ones that fit.
	if !slices.Equal(written[:len(want)], want) {
		t.Fatalf("served records written = %v, want all %d first and in order", written[:min(len(written), 5)], len(want))
	}
	if refused := written[len(want):]; len(refused) != refusedCap {
		t.Fatalf("refused records written = %d, want the %d that fit", len(refused), refusedCap)
	}
}

type recordHook struct{}

func (recordHook) Name() string { return Namespace }

func (recordHook) Fill(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
	return &Record{RequestID: lc.RequestID}, nil
}

// The collector sends a Record to the refused queue when its request never
// reached an upstream, and to the main queue when it did.
func TestSinkCollector_RoutesByUpstreamReached(t *testing.T) {
	sink := newRecordingSink()
	e := NewEmitter(EmitterOptions{Logger: testLogger()}, sink)
	reg := lifecycle.New()
	reg.RegisterHook(recordHook{})
	reg.RegisterCollector(NewSinkCollector(e))

	e.Emit(Record{RequestID: "parked"})
	<-sink.entered

	refused := lifecycle.NewContext("refused", "pipeline", time.Now())
	reg.Finalize(context.Background(), refused, &lifecycle.PostFlightEvent{ErrorKind: "rate_limited"})
	served := lifecycle.NewContext("served", "pipeline", time.Now().Add(-time.Millisecond))
	served.MarkUpstreamStart()
	reg.Finalize(context.Background(), served, &lifecycle.PostFlightEvent{Status: 200})

	if e.QueueDepth() != 1 || e.RefusedQueueDepth() != 1 {
		t.Fatalf("queue depth = %d, refused queue depth = %d, want 1 and 1", e.QueueDepth(), e.RefusedQueueDepth())
	}
	close(sink.release)
	e.Close()
	if got := strings.Join(sink.ids(), ","); got != "parked,served,refused" {
		t.Fatalf("written = %s, want parked,served,refused", got)
	}
}
