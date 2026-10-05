package payloadlog

import (
	"sync"
	"testing"
)

// parkingSink signals each Write entry and then blocks, so a test can hold the drain goroutine and watch the queue fill.
type parkingSink struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (s *parkingSink) Write(Record) error {
	s.entered <- struct{}{}
	<-s.release
	return nil
}

func TestEmitter_TryEmitRefusesInsteadOfDropping(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 16)
	e := NewEmitter(EmitterOptions{QueueSize: 2, Logger: testLogger()}, &parkingSink{entered: entered, release: block})
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(func() {
		release()
		e.Close()
	})

	if e.Capacity() != 2 || e.Free() != 2 {
		t.Fatalf("fresh emitter capacity = %d free = %d, want 2 and 2", e.Capacity(), e.Free())
	}
	e.Emit(Record{RequestID: "parked"})
	<-entered
	if !e.TryEmit(Record{RequestID: "a"}) || e.Free() != 1 {
		t.Fatalf("first TryEmit refused or free = %d, want accepted and 1", e.Free())
	}
	if !e.TryEmit(Record{RequestID: "b"}) || e.Free() != 0 {
		t.Fatalf("second TryEmit refused or free = %d, want accepted and 0", e.Free())
	}
	if e.TryEmit(Record{RequestID: "c"}) {
		t.Fatal("TryEmit accepted a record into a full queue")
	}
	if e.Dropped() != 0 {
		t.Fatalf("a refused TryEmit counted %d drops, want 0", e.Dropped())
	}

	release()
	e.Close()
	if e.TryEmit(Record{RequestID: "late"}) {
		t.Fatal("TryEmit accepted a record after Close")
	}
}
