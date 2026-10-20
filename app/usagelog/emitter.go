package usagelog

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/pkg/metrics"
)

// DefaultQueueSize is the default bounded-channel capacity if
// EmitterOptions.QueueSize is zero: about a second of arrivals at high load,
// so a drain stall (slow disk, sink swap) doesn't turn into dropped events.
// The channel holds events by value (about 0.5 KiB each), so this
// preallocates about 8 MiB.
const DefaultQueueSize = 16_384

// drainBatchMax caps how many queued events one sink call carries.
const drainBatchMax = 512

// EmitterOptions tunes Emitter behavior. Zero values are sensible
// defaults for typical deployments.
type EmitterOptions struct {
	// QueueSize is the bounded-channel capacity. <= 0 → DefaultQueueSize.
	QueueSize int

	// Logger is used for drop / sink-error warnings. nil → slog.Default.
	Logger *slog.Logger
}

// Emitter is the fan-out point: hooks Emit() events; a single
// background goroutine drains the queue in batches and writes each batch
// to every Sink (WriteBatch when the Sink implements BatchWriter).
//
// Drop-on-full preserves the "post-flight never blocks" invariant.
// Drop counter is exposed via Dropped() for /metrics or assertions.
type Emitter struct {
	queue chan Event
	stop  chan struct{}
	sinks []Sink
	log   *slog.Logger

	wg      sync.WaitGroup
	stopped atomic.Bool
	dropped atomic.Uint64
}

// NewEmitter constructs an Emitter and starts its drain goroutine.
// The Emitter must be Closed at shutdown to flush in-flight events.
func NewEmitter(opts EmitterOptions, sinks ...Sink) *Emitter {
	qsize := opts.QueueSize
	if qsize <= 0 {
		qsize = DefaultQueueSize
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	e := &Emitter{
		queue: make(chan Event, qsize),
		stop:  make(chan struct{}),
		sinks: sinks,
		log:   log,
	}
	e.wg.Add(1)
	go e.drain()
	return e
}

// Emit queues ev for delivery to all sinks. Non-blocking; if the queue
// is full the event is dropped and the drop counter increments. Safe
// for concurrent calls — the underlying channel handles fan-in.
func (e *Emitter) Emit(ev Event) {
	if e.stopped.Load() {
		return
	}
	select {
	case e.queue <- ev:
	default:
		metrics.RecordLost("usage")
		n := e.dropped.Add(1)
		// Warn once per power-of-2 to avoid log spam under sustained drop.
		if n == 1 || n&(n-1) == 0 {
			e.log.Warn("usagelog: queue full, event dropped",
				"total_dropped", n,
				"queue_size", cap(e.queue),
			)
		}
	}
}

// TryEmit queues ev like Emit, but reports false instead of dropping it when the queue is full or the emitter is closed. For callers that can refuse their own input and have it sent again; nothing is counted as lost.
func (e *Emitter) TryEmit(ev Event) bool {
	if e.stopped.Load() {
		return false
	}
	select {
	case e.queue <- ev:
		return true
	default:
		return false
	}
}

// Free returns how many more events the queue takes before Emit starts dropping. Concurrent emits make it a snapshot, not a reservation.
func (e *Emitter) Free() int { return cap(e.queue) - len(e.queue) }

// Capacity returns the queue's size.
func (e *Emitter) Capacity() int { return cap(e.queue) }

// Dropped returns the cumulative count of events dropped due to a
// full queue. Useful for /metrics scraping.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// QueueDepth returns the number of events waiting in the bounded queue —
// the leading signal before Dropped starts counting. Safe to call
// concurrently (chan len).
func (e *Emitter) QueueDepth() int { return len(e.queue) }

// Close signals the drain goroutine to finish, drains pending events,
// and returns once all sinks have processed everything in flight. After
// the queue drains, any sink implementing Closer is closed (flush final
// batch, close remote conn) so buffered/remote backends lose nothing on
// graceful shutdown. Subsequent Emit calls are no-ops. The queue is never
// closed: an Emit racing Close would panic on the send.
func (e *Emitter) Close() {
	if e.stopped.Swap(true) {
		return
	}
	close(e.stop)
	e.wg.Wait()
	for _, sink := range e.sinks {
		if c, ok := sink.(Closer); ok {
			if err := c.Close(); err != nil {
				e.log.Warn("usagelog: sink close failed", "err", err)
			}
		}
	}
}

func (e *Emitter) drain() {
	defer e.wg.Done()
	batch := make([]Event, 0, drainBatchMax)
	for {
		select {
		case ev := <-e.queue:
			batch = e.fillBatch(append(batch[:0], ev))
			e.write(batch)
		case <-e.stop:
			// Flush what was queued before Close; later racing Emits are lost.
			for {
				batch = e.fillBatch(batch[:0])
				if len(batch) == 0 {
					return
				}
				e.write(batch)
			}
		}
	}
}

// fillBatch appends whatever is already queued, up to the batch capacity,
// without waiting for more.
func (e *Emitter) fillBatch(batch []Event) []Event {
	for len(batch) < cap(batch) {
		select {
		case ev := <-e.queue:
			batch = append(batch, ev)
		default:
			return batch
		}
	}
	return batch
}

func (e *Emitter) write(batch []Event) {
	for _, sink := range e.sinks {
		if bw, ok := sink.(BatchWriter); ok {
			if err := bw.WriteBatch(batch); err != nil {
				e.log.Warn("usagelog: sink batch write failed",
					"err", err,
					"events", len(batch),
				)
			}
			continue
		}
		for i := range batch {
			if err := sink.Write(batch[i]); err != nil {
				e.log.Warn("usagelog: sink write failed",
					"err", err,
					"request_id", batch[i].RequestID,
				)
			}
		}
	}
	// Release the events' maps and strings; the slice is reused.
	clear(batch)
}
