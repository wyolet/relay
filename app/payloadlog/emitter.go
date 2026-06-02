package payloadlog

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/pkg/metrics"
)

// DefaultQueueSize is the bounded-channel capacity when EmitterOptions
// leaves it zero. Payload Records are larger than usage events, so the
// default is smaller — drop-on-full still protects the post-flight
// goroutine; a dropped payload is acceptable (it's an audit best-effort,
// not billing truth).
const DefaultQueueSize = 256

// EmitterOptions tunes Emitter behavior. Zero values are sane defaults.
type EmitterOptions struct {
	// QueueSize is the bounded-channel capacity. <= 0 → DefaultQueueSize.
	QueueSize int

	// Logger is used for drop / sink-error warnings. nil → slog.Default.
	Logger *slog.Logger
}

// Emitter fans captured Records to its Sinks via a single drain
// goroutine. Drop-on-full preserves the "post-flight never blocks"
// invariant; the drop count is exposed via Dropped().
type Emitter struct {
	queue chan Record
	sinks []Sink
	log   *slog.Logger

	wg      sync.WaitGroup
	stopped atomic.Bool
	dropped atomic.Uint64
}

// NewEmitter constructs an Emitter and starts its drain goroutine. Close
// it at shutdown to flush in-flight Records.
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
		queue: make(chan Record, qsize),
		sinks: sinks,
		log:   log,
	}
	e.wg.Add(1)
	go e.drain()
	return e
}

// Emit queues r for delivery. Non-blocking; a full queue drops the
// Record and increments the drop counter. Safe for concurrent calls.
func (e *Emitter) Emit(r Record) {
	if e.stopped.Load() {
		return
	}
	select {
	case e.queue <- r:
	default:
		metrics.RecordLost("payload")
		n := e.dropped.Add(1)
		if n == 1 || n&(n-1) == 0 {
			e.log.Warn("payloadlog: queue full, record dropped",
				"total_dropped", n,
				"queue_size", cap(e.queue),
			)
		}
	}
}

// Dropped returns the cumulative count of Records dropped due to a full
// queue.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// Close drains pending Records, then closes any Closer sink. Subsequent
// Emit calls are no-ops.
func (e *Emitter) Close() {
	if e.stopped.Swap(true) {
		return
	}
	close(e.queue)
	e.wg.Wait()
	for _, sink := range e.sinks {
		if c, ok := sink.(Closer); ok {
			if err := c.Close(); err != nil {
				e.log.Warn("payloadlog: sink close failed", "err", err)
			}
		}
	}
}

func (e *Emitter) drain() {
	defer e.wg.Done()
	for r := range e.queue {
		for _, sink := range e.sinks {
			if err := sink.Write(r); err != nil {
				e.log.Warn("payloadlog: sink write failed",
					"err", err,
					"request_id", r.RequestID,
				)
			}
		}
	}
}
