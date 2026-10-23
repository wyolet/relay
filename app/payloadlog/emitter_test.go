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
	size := recordBytes(Record{RequestID: "p"})
	e := NewEmitter(EmitterOptions{QueueBytes: 3 * size, Logger: testLogger()}, &parkingSink{entered: entered, release: block})
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(func() {
		release()
		e.Close()
	})

	if e.Capacity() != 3*size || e.Free() != 3*size {
		t.Fatalf("fresh emitter capacity = %d free = %d, want %d and %d", e.Capacity(), e.Free(), 3*size, 3*size)
	}
	e.Emit(Record{RequestID: "p"})
	<-entered
	// The Record being written still holds its bytes.
	if e.Free() != 2*size {
		t.Fatalf("free while one record is written = %d, want %d", e.Free(), 2*size)
	}
	if !e.TryEmit(Record{RequestID: "a"}) || e.Free() != size {
		t.Fatalf("first TryEmit refused or free = %d, want accepted and %d", e.Free(), size)
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

// batchSink parks its first call until release closes, then records every batch it is handed.
type batchSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	batches [][]string
}

func (s *batchSink) Write(r Record) error { return s.WriteBatch([]Record{r}) }

func (s *batchSink) WriteBatch(rs []Record) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	ids := make([]string, len(rs))
	for i := range rs {
		ids[i] = rs[i].RequestID
	}
	s.mu.Lock()
	s.batches = append(s.batches, ids)
	s.mu.Unlock()
	return nil
}

// The budget is bytes, not records: many small Records fit where one large
// body does not, the large one is dropped and counted, and everything
// queued behind a stalled write reaches the sink in one batch.
func TestEmitter_BoundsQueueByBytes(t *testing.T) {
	sink := &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
	small := Record{RequestID: "small", RequestBody: make([]byte, 1<<10)}
	budget := 1000 * recordBytes(small)
	e := NewEmitter(EmitterOptions{QueueBytes: budget, Logger: testLogger()}, sink)

	e.Emit(Record{RequestID: "parked"})
	<-sink.entered
	for range 900 {
		e.Emit(small)
	}
	e.Emit(Record{RequestID: "large", RequestBody: make([]byte, budget/2)})
	if e.Dropped() != 1 {
		t.Fatalf("dropped = %d, want only the large record", e.Dropped())
	}
	if e.QueueDepth() != 900 {
		t.Fatalf("queue depth = %d, want 900", e.QueueDepth())
	}

	close(sink.release)
	e.Close()
	if len(sink.batches) != 2 || len(sink.batches[0]) != 1 || len(sink.batches[1]) != 900 {
		sizes := make([]int, len(sink.batches))
		for i, b := range sink.batches {
			sizes[i] = len(b)
		}
		t.Fatalf("batch sizes = %v, want [1 900]", sizes)
	}
}

// An empty queue takes a Record larger than its whole budget rather than
// never storing bodies that size.
func TestEmitter_EmptyQueueTakesOversizeRecord(t *testing.T) {
	sink := &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
	close(sink.release)
	e := NewEmitter(EmitterOptions{QueueBytes: 1 << 10, Logger: testLogger()}, sink)
	e.Emit(Record{RequestID: "big", RequestBody: make([]byte, 64<<10)})
	e.Close()
	if e.Dropped() != 0 || len(sink.batches) != 1 {
		t.Fatalf("dropped = %d, batches = %d, want 0 and 1", e.Dropped(), len(sink.batches))
	}
}
