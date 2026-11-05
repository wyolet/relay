package payloadlog

import (
	"fmt"
	"testing"
	"time"
)

func newOpenBatchSink() *batchSink {
	s := &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
	close(s.release)
	return s
}

func (s *batchSink) counts() (batches, records int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		records += len(b)
	}
	return len(s.batches), records
}

// trickleWrites emits n Records one interval apart and returns how many sink
// calls the drain made for them.
func trickleWrites(t *testing.T, linger time.Duration, n int, interval time.Duration) int {
	t.Helper()
	sink := newOpenBatchSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, linger, sink)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := range n {
		<-tick.C
		e.Emit(Record{RequestID: fmt.Sprintf("t%d", i), RequestBody: []byte(`{}`)})
	}
	e.Close()
	batches, records := sink.counts()
	if records != n {
		t.Fatalf("linger %v: written %d records, want %d", linger, records, n)
	}
	return batches
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

// Close writes what is queued at once rather than waiting out the linger.
func TestEmitter_CloseSkipsLinger(t *testing.T) {
	sink := newOpenBatchSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, time.Hour, sink)
	e.Emit(Record{RequestID: "a"})
	// Give the drain time to wake and start lingering.
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	e.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with an hour-long linger, want it to skip the wait", d)
	}
	if _, records := sink.counts(); records != 1 {
		t.Fatalf("written after Close = %d, want 1", records)
	}
}

// Once drainLingerBytes are queued the batch is written without waiting
// for the linger.
func TestEmitter_LingerEndsAtByteThreshold(t *testing.T) {
	sink := newOpenBatchSink()
	e := newEmitter(EmitterOptions{Logger: testLogger()}, time.Hour, sink)
	t.Cleanup(e.Close)
	e.Emit(Record{RequestID: "small"})
	e.Emit(Record{RequestID: "large", RequestBody: make([]byte, drainLingerBytes)})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, records := sink.counts(); records == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("queued records past drainLingerBytes were not written before Close")
		}
		time.Sleep(time.Millisecond)
	}
}
