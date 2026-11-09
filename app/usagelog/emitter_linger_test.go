package usagelog

import (
	"fmt"
	"testing"
	"time"
)

// trickleWrites emits n events one interval apart and returns how many sink
// calls the drain made for them.
func trickleWrites(t *testing.T, linger time.Duration, n int, interval time.Duration) int {
	t.Helper()
	sink := newBatchSink()
	close(sink.release)
	e := newEmitter(EmitterOptions{Logger: testLogger()}, linger, sink)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := range n {
		<-tick.C
		e.Emit(served(fmt.Sprintf("t%d", i)))
	}
	e.Close()
	sizes, written := sink.result()
	if len(written) != n {
		t.Fatalf("linger %v: written %d events, want %d", linger, len(written), n)
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
	sink := newBatchSink()
	close(sink.release)
	e := newEmitter(EmitterOptions{Logger: testLogger()}, time.Hour, sink)
	e.Emit(served("a"))
	for e.QueueDepth() != 0 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	e.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with an hour-long linger, want it to skip the wait", d)
	}
	if _, written := sink.result(); len(written) != 1 {
		t.Fatalf("written after Close = %d, want 1", len(written))
	}
}

// A full batch is written without waiting for the linger, and while that
// write stalls the queue still drops on full exactly as before.
func TestEmitter_LingerKeepsDropOnFull(t *testing.T) {
	const (
		queueSize = drainBatchMax
		overflow  = 88
	)
	sink := newBatchSink()
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
	if len(sizes) != 2 || sizes[0] != drainBatchMax || len(written) != drainBatchMax+queueSize {
		t.Fatalf("batch sizes = %v, want two full batches", sizes)
	}
}
