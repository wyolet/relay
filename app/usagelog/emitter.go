package usagelog

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// DefaultQueueSize is the default bounded-channel capacity if
// EmitterOptions.QueueSize is zero. 1024 events comfortably absorbs a
// brief drain stall (sink slow / disk flush / pipe back-pressure)
// without blocking the post-flight goroutine.
const DefaultQueueSize = 1024

// EmitterOptions tunes Emitter behavior. Zero values are sensible
// defaults for typical deployments.
type EmitterOptions struct {
	// QueueSize is the bounded-channel capacity. <= 0 → DefaultQueueSize.
	QueueSize int

	// Logger is used for drop / sink-error warnings. nil → slog.Default.
	Logger *slog.Logger
}

// Emitter is the fan-out point: hooks Emit() events; a single
// background goroutine drains the queue and writes to each Sink.
//
// Drop-on-full preserves the "post-flight never blocks" invariant.
// Drop counter is exposed via Dropped() for /metrics or assertions.
type Emitter struct {
	queue chan Event
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

// Dropped returns the cumulative count of events dropped due to a
// full queue. Useful for /metrics scraping.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// Close signals the drain goroutine to finish, drains pending events,
// and returns once all sinks have processed everything in flight.
// Subsequent Emit calls are no-ops.
func (e *Emitter) Close() {
	if e.stopped.Swap(true) {
		return
	}
	close(e.queue)
	e.wg.Wait()
}

func (e *Emitter) drain() {
	defer e.wg.Done()
	for ev := range e.queue {
		for _, sink := range e.sinks {
			if err := sink.Write(ev); err != nil {
				e.log.Warn("usagelog: sink write failed",
					"err", err,
					"request_id", ev.RequestID,
				)
			}
		}
	}
}
