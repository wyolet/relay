package payloadlog

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

func rec(id string) Record { return Record{RequestID: id} }

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// batchSink records each batch it receives. The first call of a parked sink
// signals entered and waits on release, so a test can hold the drain
// goroutine and watch the queue fill behind it.
type batchSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error

	mu      sync.Mutex
	sizes   []int
	records []Record
	closed  int
}

func parkedSink() *batchSink {
	return &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func openSink() *batchSink {
	s := parkedSink()
	close(s.release)
	return s
}

func (s *batchSink) Write(r Record) error { return s.WriteBatch([]Record{r}) }

func (s *batchSink) WriteBatch(rs []Record) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, len(rs))
	s.records = append(s.records, rs...)
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
	for _, r := range s.records {
		written = append(written, r.RequestID)
	}
	return slices.Clone(s.sizes), written
}

// writeOnly hides a sink's WriteBatch and Close, so the emitter must fall back to Write.
type writeOnly struct{ s *batchSink }

func (w writeOnly) Write(r Record) error { return w.s.Write(r) }

func lost(kind string) float64 {
	return testutil.ToFloat64(metrics.RecordsLost.WithLabelValues(kind))
}

// Every sink receives every Record whole, served ones before refused ones,
// whether it takes batches or single writes; a failing sink does not starve
// the next. Close flushes, closes each closable sink once, and turns later
// emits into no-ops.
func TestEmitter_FansOutToEverySink(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	records := []Record{
		{RequestID: "r1", Timestamp: at, ProjectID: "p", PrincipalID: "u", RelayKeyHash: "h", RequestBody: []byte(`{"in":1}`), ResponseBody: []byte(`{"out":2}`), ResponseTruncated: true},
		{RequestID: "r2", Timestamp: at, RequestBody: []byte("x"), RequestTruncated: true},
		{RequestID: "r3", Timestamp: at},
	}
	failing, batching, single := parkedSink(), openSink(), openSink()
	failing.err = errors.New("sink down")
	e := NewEmitter(EmitterOptions{Logger: testLogger()}, failing, batching, writeOnly{single})

	e.Emit(records[0])
	<-failing.entered
	e.EmitRefused(records[2])
	e.Emit(records[1])
	close(failing.release)
	e.Close()
	e.Close()
	e.Emit(rec("late"))
	e.EmitRefused(rec("late"))
	if e.TryEmit(rec("late")) {
		t.Fatal("TryEmit accepted a record after Close")
	}

	for name, s := range map[string]*batchSink{"failing": failing, "batching": batching, "write-only": single} {
		s.mu.Lock()
		if diff := cmp.Diff(records, s.records); diff != "" {
			t.Errorf("%s sink (-want +got):\n%s", name, diff)
		}
		s.mu.Unlock()
	}
	if sizes, _ := batching.result(); !slices.Equal(sizes, []int{1, 1, 1}) {
		t.Errorf("batch sizes = %v, want the parked record, then the served queue, then the refused one", sizes)
	}
	if failing.closed != 1 || batching.closed != 1 || single.closed != 0 {
		t.Errorf("closes = %d, %d, %d; want each closable sink closed once", failing.closed, batching.closed, single.closed)
	}
	if e.Dropped() != 0 || e.DroppedRefused() != 0 || e.QueueDepth() != 0 || e.RefusedQueueDepth() != 0 || e.Free() != e.Capacity() {
		t.Errorf("after Close: dropped %d/%d, depth %d/%d, free %d of %d", e.Dropped(), e.DroppedRefused(), e.QueueDepth(), e.RefusedQueueDepth(), e.Free(), e.Capacity())
	}
}

// The budget is bytes, not records: many small Records fit where one large
// body does not, what does not fit is dropped and counted under its queue's
// kind, and everything queued behind a stalled write reaches the sink.
func TestEmitter_BoundsEachQueueByBytes(t *testing.T) {
	lostServed, lostRefused := lost("payload"), lost("payload_refused")
	sink := parkedSink()
	small := Record{RequestID: "small", RequestBody: make([]byte, 1<<10)}
	budget := 1000 * recordBytes(small)
	e := NewEmitter(EmitterOptions{QueueBytes: budget, RefusedQueueBytes: 2 * recordBytes(small), Logger: testLogger()}, sink)
	e.Emit(rec("parked"))
	<-sink.entered

	for range 900 {
		e.Emit(small)
	}
	e.Emit(Record{RequestID: "large", RequestBody: make([]byte, budget/2)})
	for range 5 {
		e.EmitRefused(small)
	}
	if e.Dropped() != 1 || e.QueueDepth() != 900 || lost("payload")-lostServed != 1 {
		t.Fatalf("served: dropped %d, depth %d, metric +%v; want only the large record dropped", e.Dropped(), e.QueueDepth(), lost("payload")-lostServed)
	}
	if e.DroppedRefused() != 3 || e.RefusedQueueDepth() != 2 || lost("payload_refused")-lostRefused != 3 {
		t.Fatalf("refused: dropped %d, depth %d, metric +%v; want 3, 2, 3", e.DroppedRefused(), e.RefusedQueueDepth(), lost("payload_refused")-lostRefused)
	}

	close(sink.release)
	e.Close()
	if sizes, _ := sink.result(); !slices.Equal(sizes, []int{1, 900, 2}) {
		t.Fatalf("batch sizes = %v, want [1 900 2]", sizes)
	}
}

// An empty queue takes a Record larger than its whole budget rather than
// never storing bodies that size; the next one waits for it to be written.
func TestEmitter_EmptyQueueTakesOversizeRecord(t *testing.T) {
	sink := parkedSink()
	e := NewEmitter(EmitterOptions{QueueBytes: 1 << 10, RefusedQueueBytes: 1 << 10, Logger: testLogger()}, sink)
	big := Record{RequestID: "big", RequestBody: make([]byte, 64<<10)}
	e.Emit(big)
	<-sink.entered
	// The record being written still holds its bytes.
	e.Emit(rec("behind"))
	e.EmitRefused(big)
	e.EmitRefused(rec("behind-refused"))
	if e.Dropped() != 1 || e.DroppedRefused() != 1 || e.Free() != 1<<10-recordBytes(big) {
		t.Fatalf("dropped %d/%d, free %d; want one drop behind each oversize record", e.Dropped(), e.DroppedRefused(), e.Free())
	}
	close(sink.release)
	e.Close()
	if _, written := sink.result(); !slices.Equal(written, []string{"big", "big"}) {
		t.Fatalf("written = %v, want the two oversize records", written)
	}
}

func TestEmitter_TryEmitRefusesInsteadOfDropping(t *testing.T) {
	lostServed := lost("payload")
	sink := parkedSink()
	size := recordBytes(rec("p"))
	e := NewEmitter(EmitterOptions{QueueBytes: 3 * size, Logger: testLogger()}, sink)
	if e.Capacity() != 3*size || e.Free() != 3*size {
		t.Fatalf("fresh emitter capacity = %d free = %d, want %d and %d", e.Capacity(), e.Free(), 3*size, 3*size)
	}
	e.Emit(rec("p"))
	<-sink.entered

	// The Record being written still holds its bytes.
	if e.Free() != 2*size {
		t.Fatalf("free while one record is written = %d, want %d", e.Free(), 2*size)
	}
	if !e.TryEmit(rec("a")) || e.Free() != size {
		t.Fatalf("first TryEmit refused or free = %d, want accepted and %d", e.Free(), size)
	}
	if !e.TryEmit(rec("b")) || e.Free() != 0 {
		t.Fatalf("second TryEmit refused or free = %d, want accepted and 0", e.Free())
	}
	if e.TryEmit(rec("c")) {
		t.Fatal("TryEmit accepted a record into a full queue")
	}
	if e.Dropped() != 0 || lost("payload") != lostServed {
		t.Fatalf("a refused TryEmit counted as lost: dropped %d", e.Dropped())
	}
	close(sink.release)
	e.Close()
	if _, written := sink.result(); !slices.Equal(written, []string{"p", "a", "b"}) {
		t.Fatalf("written = %v, want the accepted records only", written)
	}
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
	lostServed, lostRefused := lost("payload"), lost("payload_refused")
	sink := parkedSink()
	refusedBytes := refusedCap * recordBytes(Record{RequestID: "refused-0-0000", RequestBody: []byte(`{}`)})
	e := NewEmitter(EmitterOptions{QueueBytes: 1 << 20, RefusedQueueBytes: refusedBytes, Logger: testLogger()}, sink)
	e.Emit(rec("parked"))
	<-sink.entered

	var wg sync.WaitGroup
	for f := range flooders {
		wg.Go(func() {
			for i := range perFlooder {
				e.EmitRefused(Record{RequestID: fmt.Sprintf("refused-%d-%04d", f, i), RequestBody: []byte(`{}`)})
			}
		})
	}
	want := append([]string{"parked"}, ids("served-", servedN)...)
	for _, id := range want[1:] {
		e.Emit(rec(id))
	}
	wg.Wait()

	if e.Dropped() != 0 || lost("payload") != lostServed {
		t.Fatalf("served records dropped = %d, want 0", e.Dropped())
	}
	wantDrops := uint64(flooders*perFlooder - refusedCap)
	if got := e.DroppedRefused(); got != wantDrops || lost("payload_refused")-lostRefused != float64(wantDrops) {
		t.Fatalf("refused records dropped = %d (metric +%v), want %d", got, lost("payload_refused")-lostRefused, wantDrops)
	}

	close(sink.release)
	e.Close()
	_, written := sink.result()
	// Served Records drain strictly first, then the refused ones that fit.
	if len(written) != len(want)+refusedCap || !slices.Equal(written[:len(want)], want) {
		t.Fatalf("written %d records, want all %d served first and in order, then %d refused", len(written), len(want), refusedCap)
	}
	if i := slices.IndexFunc(written[len(want):], func(id string) bool { return !strings.HasPrefix(id, "refused-") }); i >= 0 {
		t.Fatalf("a served record was written after refused ones: %q", written[len(want)+i])
	}
}

// trickleWrites emits n Records one interval apart and returns how many sink
// calls the drain made for them.
func trickleWrites(t *testing.T, linger time.Duration, n int, interval time.Duration) int {
	t.Helper()
	sink := openSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, linger, sink)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := range n {
		<-tick.C
		e.Emit(Record{RequestID: fmt.Sprintf("t%d", i), RequestBody: []byte(`{}`)})
	}
	e.Close()
	sizes, written := sink.result()
	if !slices.Equal(written, ids("t", n)) {
		t.Fatalf("linger %v: written %d records, want %d in order", linger, len(written), n)
	}
	return len(sizes)
}

// A steady trickle shares writes once the drain lingers, instead of one
// sink call per Record.
func TestEmitter_LingerBatchesTrickle(t *testing.T) {
	const (
		n        = 100
		interval = time.Millisecond
	)
	without := trickleWrites(t, 0, n, interval)
	with := trickleWrites(t, drainLinger, n, interval)
	t.Logf("%d records %v apart: %d sink writes without linger, %d with %v", n, interval, without, with, drainLinger)
	if with > n/3 || with*2 > without {
		t.Fatalf("sink writes with linger = %d, without = %d; want under %d and under half", with, without, n/3)
	}
}

// A lingering batch is written as soon as drainLingerBytes are queued, and
// Close writes whatever is still waiting rather than sitting out the linger.
func TestEmitter_LingerEndsAtByteThresholdOrClose(t *testing.T) {
	sink := openSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, time.Hour, sink)
	written := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, got := sink.result(); slices.Equal(got, want) {
				return
			}
			if time.Now().After(deadline) {
				_, got := sink.result()
				t.Fatalf("written = %v, want %v", got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	e.Emit(rec("small"))
	e.Emit(Record{RequestID: "large", RequestBody: make([]byte, drainLingerBytes)})
	written("small", "large")

	// Give the drain time to go back to waiting, then to start lingering on the tail.
	time.Sleep(10 * time.Millisecond)
	e.Emit(rec("tail"))
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	e.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with an hour-long linger, want it to skip the wait", d)
	}
	written("small", "large", "tail")
}

// Emits run on detached post-flight goroutines nothing awaits, so an Emit
// concurrent with Close must be a safe no-op/drop — never a panic.
func TestEmitterEmitConcurrentWithCloseDoesNotPanic(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS > 1 to interleave Emit and Close")
	}
	// Fewer spinners than CPUs, so Close and the drain goroutine are not
	// starved waiting on preemption.
	workers := min(runtime.GOMAXPROCS(0)-1, 3)
	const trials = 2000
	for trial := range trials {
		e := NewEmitter(EmitterOptions{QueueBytes: 64 * recordBytes(Record{}), Logger: testLogger()})
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
				e.Emit(Record{})
				ready.Done()
				for !stop.Load() {
					e.Emit(Record{})
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
