package usagelog

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// served is an event of a request that reached an upstream, so it waits in
// the main queue; refused is one that did not.
func served(id string) Event  { return Event{RequestID: id, Upstream: &UpstreamTiming{Start: 1}} }
func refused(id string) Event { return Event{RequestID: id, Status: 429, ErrorKind: "overloaded"} }

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// batchSink records each batch it receives. The first call of a parked sink
// signals entered and waits on release, so a test can hold the drain
// goroutine and queue a burst behind it.
type batchSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error

	mu     sync.Mutex
	sizes  []int
	events []Event
	closed int
}

func parkedSink() *batchSink {
	return &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func openSink() *batchSink {
	s := parkedSink()
	close(s.release)
	return s
}

func (s *batchSink) Write(ev Event) error { return s.WriteBatch([]Event{ev}) }

func (s *batchSink) WriteBatch(evs []Event) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, len(evs))
	s.events = append(s.events, evs...)
	return s.err
}

func (s *batchSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return s.err
}

func (s *batchSink) result() (sizes []int, written []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		written = append(written, ev.RequestID)
	}
	return slices.Clone(s.sizes), written
}

// writeOnly hides a sink's WriteBatch and Close, so the emitter must fall back to Write.
type writeOnly struct{ s *batchSink }

func (w writeOnly) Write(ev Event) error { return w.s.Write(ev) }

func lost(kind string) float64 {
	return testutil.ToFloat64(metrics.RecordsLost.WithLabelValues(kind))
}

// Every sink receives every event whole and in order, whether it takes
// batches or single writes, and a sink that fails does not starve the next.
// Close flushes, closes each closable sink once, and turns later emits into
// no-ops.
func TestEmitter_FansOutToEverySink(t *testing.T) {
	cost := int64(42)
	events := []Event{
		{RequestID: "r1", Source: "pipeline", Status: 200, DurationMs: 42, Upstream: &UpstreamTiming{Start: 3, ResponseStart: 7, ResponseEnd: 9},
			Tokens: map[string]int64{"input": 5}, CostNanos: &cost, Tags: map[string]string{"k": "v"}},
		{RequestID: "r2", Source: "proxy", Status: 429, ErrorKind: "upstream_error", ErrorMessage: "upstream returned 429", Upstream: &UpstreamTiming{Start: 2}},
		{RequestID: "r3", Source: "batch", Status: 502, ErrorKind: "upstream_error", Upstream: &UpstreamTiming{Start: 1}},
	}
	failing, batching, single := parkedSink(), openSink(), openSink()
	failing.err = errors.New("sink down")
	e := NewEmitter(EmitterOptions{Logger: testLogger()}, failing, batching, writeOnly{single})

	e.Emit(events[0])
	<-failing.entered
	e.Emit(events[1])
	e.Emit(events[2])
	close(failing.release)
	e.Close()
	e.Close()
	e.Emit(served("late"))
	if e.TryEmit(served("late")) {
		t.Fatal("TryEmit accepted an event after Close")
	}

	for name, s := range map[string]*batchSink{"failing": failing, "batching": batching, "write-only": single} {
		s.mu.Lock()
		if diff := cmp.Diff(events, s.events); diff != "" {
			t.Errorf("%s sink (-want +got):\n%s", name, diff)
		}
		s.mu.Unlock()
	}
	if sizes, _ := batching.result(); !slices.Equal(sizes, []int{1, 2}) {
		t.Errorf("batch sizes = %v, want the parked event then the two queued behind it", sizes)
	}
	if sizes, _ := single.result(); !slices.Equal(sizes, []int{1, 1, 1}) {
		t.Errorf("write-only sink call sizes = %v, want one Write per event", sizes)
	}
	if failing.closed != 1 || batching.closed != 1 || single.closed != 0 {
		t.Errorf("closes = %d, %d, %d; want each closable sink closed once", failing.closed, batching.closed, single.closed)
	}
	if e.Dropped() != 0 || e.DroppedRefused() != 0 || e.QueueDepth() != 0 || e.RefusedQueueDepth() != 0 {
		t.Errorf("after Close: dropped %d/%d, depth %d/%d; want all zero", e.Dropped(), e.DroppedRefused(), e.QueueDepth(), e.RefusedQueueDepth())
	}
}

// With the sink stalled each queue fills to its capacity and every event past
// it is dropped and counted under its own kind; what fitted is still written.
func TestEmitter_DropsOnFullQueue(t *testing.T) {
	lostUsage, lostRefused := lost("usage"), lost("usage_refused")
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueSize: 4, RefusedQueueSize: 2, Logger: testLogger()}, sink)
	if e.Capacity() != 4 || e.Free() != 4 || e.QueueDepth() != 0 {
		t.Fatalf("fresh emitter: capacity %d, free %d, depth %d", e.Capacity(), e.Free(), e.QueueDepth())
	}
	e.Emit(served("parked"))
	<-sink.entered

	for _, id := range ids("s", 9) {
		e.Emit(served(id))
	}
	for _, id := range ids("r", 5) {
		e.Emit(refused(id))
	}
	if e.Dropped() != 5 || e.QueueDepth() != 4 || e.Free() != 0 {
		t.Fatalf("served: dropped %d, depth %d, free %d; want 5, 4, 0", e.Dropped(), e.QueueDepth(), e.Free())
	}
	if e.DroppedRefused() != 3 || e.RefusedQueueDepth() != 2 {
		t.Fatalf("refused: dropped %d, depth %d; want 3, 2", e.DroppedRefused(), e.RefusedQueueDepth())
	}
	if got := lost("usage") - lostUsage; got != 5 {
		t.Errorf("records lost {kind=usage} rose by %v, want 5", got)
	}
	if got := lost("usage_refused") - lostRefused; got != 3 {
		t.Errorf("records lost {kind=usage_refused} rose by %v, want 3", got)
	}

	close(sink.release)
	e.Close()
	// The drain's wakeup may pick r0 ahead of the served rows; nothing else reorders.
	_, written := sink.result()
	servedFirst := []string{"parked", "s0", "s1", "s2", "s3", "r0", "r1"}
	refusedWoke := []string{"parked", "r0", "s0", "s1", "s2", "s3", "r1"}
	if !slices.Equal(written, servedFirst) && !slices.Equal(written, refusedWoke) {
		t.Fatalf("written = %v, want %v", written, servedFirst)
	}
}

func TestEmitter_TryEmitRefusesInsteadOfDropping(t *testing.T) {
	lostUsage := lost("usage")
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueSize: 2, Logger: testLogger()}, sink)
	e.Emit(served("parked"))
	<-sink.entered

	// TryEmit takes the main queue whatever the event's upstream timing.
	if !e.TryEmit(refused("a")) || e.Free() != 1 || e.RefusedQueueDepth() != 0 {
		t.Fatalf("first TryEmit: free %d, refused depth %d; want accepted on the main queue", e.Free(), e.RefusedQueueDepth())
	}
	if !e.TryEmit(served("b")) || e.Free() != 0 {
		t.Fatalf("second TryEmit refused or free = %d, want accepted and 0", e.Free())
	}
	if e.TryEmit(served("c")) {
		t.Fatal("TryEmit accepted an event into a full queue")
	}
	if e.Dropped() != 0 || lost("usage") != lostUsage {
		t.Fatalf("a refused TryEmit counted as lost: dropped %d", e.Dropped())
	}
	close(sink.release)
	e.Close()
	if _, written := sink.result(); !slices.Equal(written, []string{"parked", "a", "b"}) {
		t.Fatalf("written = %v, want the accepted events only", written)
	}
}

func TestEmitter_DrainsBurstInBatches(t *testing.T) {
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueSize: 2000, Logger: testLogger()}, sink)
	e.Emit(served("first"))
	<-sink.entered

	burst := ids("b", 1200)
	for _, id := range burst {
		e.Emit(served(id))
	}
	if d := e.QueueDepth(); d != len(burst) {
		t.Fatalf("queue depth with drain parked = %d, want %d", d, len(burst))
	}
	close(sink.release)
	e.Close()

	sizes, written := sink.result()
	if want := append([]string{"first"}, burst...); !slices.Equal(written, want) {
		t.Fatalf("written %d events out of order or incomplete, want %d", len(written), len(want))
	}
	if want := []int{1, drainBatchMax, drainBatchMax, 1200 - 2*drainBatchMax}; !slices.Equal(sizes, want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
}

func TestEmitter_ConcurrentEmit(t *testing.T) {
	sink := openSink()
	e := NewEmitter(EmitterOptions{QueueSize: 10_000}, sink)
	var wg sync.WaitGroup
	for g := range 100 {
		wg.Go(func() {
			for j := range 10 {
				e.Emit(served(fmt.Sprintf("g%d-%d", g, j)))
			}
		})
	}
	wg.Wait()
	e.Close()

	_, written := sink.result()
	slices.Sort(written)
	if len(written) != 1000 || len(slices.Compact(written)) != 1000 || e.Dropped() != 0 {
		t.Fatalf("written %d (%d distinct), dropped %d; want 1000 distinct events and no drop", len(written), len(slices.Compact(written)), e.Dropped())
	}
}

// A flood of refused requests' events while the sink is stalled must drop only
// refusal rows: every served request's row still reaches the sink.
func TestEmitter_RefusalsNeverDisplaceServedEvents(t *testing.T) {
	const (
		servedN    = 200
		flooders   = 8
		perFlooder = 1000
		refusedCap = 16
	)
	lostUsage, lostRefused := lost("usage"), lost("usage_refused")
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueSize: servedN, RefusedQueueSize: refusedCap, Logger: testLogger()}, sink)
	e.Emit(served("parked"))
	<-sink.entered

	var wg sync.WaitGroup
	for f := range flooders {
		wg.Go(func() {
			for i := range perFlooder {
				e.Emit(refused(fmt.Sprintf("refused-%d-%d", f, i)))
			}
		})
	}
	want := append([]string{"parked"}, ids("served-", servedN)...)
	for _, id := range want[1:] {
		e.Emit(served(id))
	}
	wg.Wait()

	if e.Dropped() != 0 || lost("usage") != lostUsage {
		t.Fatalf("served events dropped = %d, want 0", e.Dropped())
	}
	wantDrops := uint64(flooders*perFlooder - refusedCap)
	if got := e.DroppedRefused(); got != wantDrops || lost("usage_refused")-lostRefused != float64(wantDrops) {
		t.Fatalf("refused events dropped = %d (metric +%v), want %d", got, lost("usage_refused")-lostRefused, wantDrops)
	}
	if d := e.RefusedQueueDepth(); d != refusedCap {
		t.Fatalf("refused queue depth = %d, want %d (full)", d, refusedCap)
	}

	close(sink.release)
	e.Close()
	_, written := sink.result()
	servedWritten := slices.DeleteFunc(slices.Clone(written), func(id string) bool { return strings.HasPrefix(id, "refused-") })
	if !slices.Equal(servedWritten, want) {
		t.Fatalf("served rows written = %d, want all %d in order", len(servedWritten), len(want))
	}
	if got := len(written) - len(servedWritten); got != refusedCap {
		t.Fatalf("refused rows written = %d, want the %d that fit", got, refusedCap)
	}
}

// The drain empties the served queue before the refused one, so a slow sink
// spends its time on billable rows first. Only the one event the drain's own
// wakeup picked may be a refused one ahead of them.
func TestEmitter_DrainsServedBeforeRefused(t *testing.T) {
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueSize: 100, RefusedQueueSize: 100, Logger: testLogger()}, sink)
	e.Emit(served("parked"))
	<-sink.entered

	servedIDs, refusedIDs := ids("served-", 50), ids("refused-", 50)
	for i := range 50 {
		e.Emit(refused(refusedIDs[i]))
		e.Emit(served(servedIDs[i]))
	}
	close(sink.release)
	e.Close()

	_, written := sink.result()
	servedFirst := slices.Concat([]string{"parked"}, servedIDs, refusedIDs)
	refusedWoke := slices.Concat([]string{"parked", refusedIDs[0]}, servedIDs, refusedIDs[1:])
	if !slices.Equal(written, servedFirst) && !slices.Equal(written, refusedWoke) {
		t.Fatalf("a served row was written after queued refused rows: %v", written)
	}
}

// trickleWrites emits n events one interval apart and returns how many sink
// calls the drain made for them.
func trickleWrites(t *testing.T, linger time.Duration, n int, interval time.Duration) int {
	t.Helper()
	sink := openSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, linger, sink)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := range n {
		<-tick.C
		e.Emit(served(fmt.Sprintf("t%d", i)))
	}
	e.Close()
	sizes, written := sink.result()
	if !slices.Equal(written, ids("t", n)) {
		t.Fatalf("linger %v: written %d events, want %d in order", linger, len(written), n)
	}
	return len(sizes)
}

// A steady trickle shares writes once the drain lingers, instead of one
// sink call per event.
func TestEmitter_LingerBatchesTrickle(t *testing.T) {
	const (
		n        = 100
		interval = time.Millisecond
	)
	without := trickleWrites(t, 0, n, interval)
	with := trickleWrites(t, drainLinger, n, interval)
	t.Logf("%d events %v apart: %d sink writes without linger, %d with %v", n, interval, without, with, drainLinger)
	if with > n/3 || with*2 > without {
		t.Fatalf("sink writes with linger = %d, without = %d; want under %d and under half", with, without, n/3)
	}
}

// Close writes a part-full batch at once rather than waiting out the linger.
func TestEmitter_CloseSkipsLinger(t *testing.T) {
	sink := openSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, time.Hour, sink)
	e.Emit(served("a"))
	for e.QueueDepth() != 0 {
		time.Sleep(time.Millisecond)
	}
	if sizes, _ := sink.result(); len(sizes) != 0 {
		t.Fatalf("a part-full batch was written before the linger ran out: %v", sizes)
	}
	start := time.Now()
	e.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with an hour-long linger, want it to skip the wait", d)
	}
	if sizes, written := sink.result(); !slices.Equal(sizes, []int{1}) || !slices.Equal(written, []string{"a"}) {
		t.Fatalf("after Close: batches %v, written %v; want the one lingering batch", sizes, written)
	}
}

// A full batch is written without waiting for the linger, and while that
// write stalls the queue still drops on full exactly as before.
func TestEmitter_LingerKeepsDropOnFull(t *testing.T) {
	const (
		queueSize = drainBatchMax
		overflow  = 88
	)
	sink := parkedSink()
	e := newEmitter(EmitterOptions{QueueSize: queueSize, Logger: testLogger()}, time.Hour, sink)
	for i := range drainBatchMax {
		e.Emit(served(fmt.Sprintf("f%d", i)))
	}
	// Only a full batch reaches the sink before Close with an hour-long linger.
	<-sink.entered
	for i := range queueSize + overflow {
		e.Emit(served(fmt.Sprintf("q%d", i)))
	}
	if e.Dropped() != overflow || e.QueueDepth() != queueSize {
		t.Fatalf("dropped = %d, depth = %d, want %d and %d", e.Dropped(), e.QueueDepth(), overflow, queueSize)
	}
	close(sink.release)
	e.Close()
	sizes, written := sink.result()
	if !slices.Equal(sizes, []int{drainBatchMax, queueSize}) || !slices.Equal(written, append(ids("f", drainBatchMax), ids("q", queueSize)...)) {
		t.Fatalf("batch sizes = %v, want two full batches holding what fitted, in order", sizes)
	}
}

// Emits run on detached post-flight goroutines nothing awaits, so an Emit
// concurrent with Close must be a safe no-op/drop — a panic there kills the
// process at shutdown. The window is concurrent-only, so this is a tight-loop
// harness with per-goroutine recover; -race widens the interleavings.
func TestEmitterEmitConcurrentWithCloseDoesNotPanic(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS > 1 to interleave Emit and Close")
	}
	// Fewer spinners than CPUs, so Close and the drain goroutine are not
	// starved waiting on preemption.
	workers := min(runtime.GOMAXPROCS(0)-1, 3)
	const trials = 2000
	for trial := range trials {
		e := NewEmitter(EmitterOptions{QueueSize: 64, Logger: testLogger()})
		var (
			panicked atomic.Value
			stop     atomic.Bool
			start    = make(chan struct{})
			ready    sync.WaitGroup
			wg       sync.WaitGroup
		)
		ready.Add(workers)
		for range workers {
			wg.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						panicked.Store(fmt.Sprint(r))
					}
				}()
				<-start
				e.Emit(Event{})
				ready.Done()
				for !stop.Load() {
					e.Emit(Event{})
				}
			})
		}
		close(start)
		ready.Wait() // all workers actively looping Emit
		e.Close()
		stop.Store(true)
		wg.Wait()
		if p := panicked.Load(); p != nil {
			t.Fatalf("trial %d: Emit racing Close must be a safe no-op/drop, but panicked: %v", trial, p)
		}
	}
}
