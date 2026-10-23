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

// DefaultRefusedQueueSize is the capacity for Records of requests that never
// reached an upstream (shed, rate-limited, refused). Refusals peak exactly
// when relay is overloaded, so they wait apart and cannot crowd out the
// Records of served requests.
const DefaultRefusedQueueSize = 32

// EmitterOptions tunes Emitter behavior. Zero values are sane defaults.
type EmitterOptions struct {
	// QueueSize is the bounded-channel capacity. <= 0 → DefaultQueueSize.
	QueueSize int

	// RefusedQueueSize is the capacity for Records of requests that never
	// reached an upstream. <= 0 → DefaultRefusedQueueSize.
	RefusedQueueSize int

	// Logger is used for drop / sink-error warnings. nil → slog.Default.
	Logger *slog.Logger
}

// Emitter fans captured Records to its Sinks via a single drain
// goroutine. Drop-on-full preserves the "post-flight never blocks"
// invariant; the drop count is exposed via Dropped().
//
// Records of requests that never reached an upstream wait in a separate,
// smaller queue that the drain reads only while the main one is empty, so a
// flood of refusals drops refusal Records, never served ones.
type Emitter struct {
	queue   chan Record
	refused chan Record
	stop    chan struct{}
	sinks   []Sink
	log     *slog.Logger

	wg             sync.WaitGroup
	stopped        atomic.Bool
	dropped        atomic.Uint64
	droppedRefused atomic.Uint64
}

// NewEmitter constructs an Emitter and starts its drain goroutine. Close
// it at shutdown to flush in-flight Records.
func NewEmitter(opts EmitterOptions, sinks ...Sink) *Emitter {
	qsize := opts.QueueSize
	if qsize <= 0 {
		qsize = DefaultQueueSize
	}
	rsize := opts.RefusedQueueSize
	if rsize <= 0 {
		rsize = DefaultRefusedQueueSize
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	e := &Emitter{
		queue:   make(chan Record, qsize),
		refused: make(chan Record, rsize),
		stop:    make(chan struct{}),
		sinks:   sinks,
		log:     log,
	}
	e.wg.Add(1)
	go e.drain()
	return e
}

// Emit queues r, a served request's Record, for delivery. Non-blocking; a
// full queue drops the Record and increments the drop counter. Safe for
// concurrent calls.
func (e *Emitter) Emit(r Record) {
	if e.stopped.Load() {
		return
	}
	select {
	case e.queue <- r:
	default:
		e.recordDrop("payload", &e.dropped, cap(e.queue))
	}
}

// EmitRefused queues r, the Record of a request that never reached an
// upstream, on the refused queue. A full queue drops it, counted as
// payload_refused.
func (e *Emitter) EmitRefused(r Record) {
	if e.stopped.Load() {
		return
	}
	select {
	case e.refused <- r:
	default:
		e.recordDrop("payload_refused", &e.droppedRefused, cap(e.refused))
	}
}

func (e *Emitter) recordDrop(kind string, counter *atomic.Uint64, size int) {
	metrics.RecordLost(kind)
	n := counter.Add(1)
	if n == 1 || n&(n-1) == 0 {
		e.log.Warn("payloadlog: queue full, record dropped",
			"queue", kind,
			"total_dropped", n,
			"queue_size", size,
		)
	}
}

// TryEmit queues r on the main queue like Emit but reports a full or closed queue instead of dropping: nothing is counted or logged, the caller decides what a refused Record means.
func (e *Emitter) TryEmit(r Record) bool {
	if e.stopped.Load() {
		return false
	}
	select {
	case e.queue <- r:
		return true
	default:
		return false
	}
}

// Free returns how many more Records the queue takes before Emit starts dropping. Concurrent emits make it a snapshot, not a reservation.
func (e *Emitter) Free() int { return cap(e.queue) - len(e.queue) }

// Capacity returns the queue's size.
func (e *Emitter) Capacity() int { return cap(e.queue) }

// Dropped returns the cumulative count of Records dropped due to a full
// queue.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// DroppedRefused returns the cumulative count of refused requests' Records
// dropped because the refused queue was full.
func (e *Emitter) DroppedRefused() uint64 { return e.droppedRefused.Load() }

// RefusedQueueDepth returns the number of refused requests' Records waiting.
func (e *Emitter) RefusedQueueDepth() int { return len(e.refused) }

// QueueDepth returns the number of Records waiting in the bounded queue —
// the leading signal before Dropped starts counting. Safe to call
// concurrently (chan len).
func (e *Emitter) QueueDepth() int { return len(e.queue) }

// Close drains pending Records, then closes any Closer sink. Subsequent
// Emit calls are no-ops. The queue is never closed: an Emit racing Close
// would panic on the send.
func (e *Emitter) Close() {
	if e.stopped.Swap(true) {
		return
	}
	close(e.stop)
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
	for {
		// Served Records first: a refused one is written only when none waits.
		select {
		case r := <-e.queue:
			e.write(r)
			continue
		default:
		}
		select {
		case r := <-e.queue:
			e.write(r)
		case r := <-e.refused:
			e.write(r)
		case <-e.stop:
			// Flush what was queued before Close; later racing Emits are lost.
			e.flush(e.queue)
			e.flush(e.refused)
			return
		}
	}
}

func (e *Emitter) flush(q <-chan Record) {
	for {
		select {
		case r := <-q:
			e.write(r)
		default:
			return
		}
	}
}

func (e *Emitter) write(r Record) {
	for _, sink := range e.sinks {
		if err := sink.Write(r); err != nil {
			e.log.Warn("payloadlog: sink write failed",
				"err", err,
				"request_id", r.RequestID,
			)
		}
	}
}
