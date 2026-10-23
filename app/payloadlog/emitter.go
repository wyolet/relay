package payloadlog

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/pkg/metrics"
)

// DefaultQueueBytes is the served queue's budget when EmitterOptions leaves
// it zero. Drop-on-full still protects the post-flight goroutine.
const DefaultQueueBytes = 16 << 20

// DefaultRefusedQueueBytes is the budget for Records of requests that never
// reached an upstream (shed, rate-limited, refused). Refusals peak exactly
// when relay is overloaded, so they wait apart and cannot crowd out the
// Records of served requests.
const DefaultRefusedQueueBytes = 2 << 20

// EmitterOptions tunes Emitter behavior. Zero values are sane defaults.
type EmitterOptions struct {
	// QueueBytes bounds the memory held by queued served Records, counted
	// by recordBytes. <= 0 → DefaultQueueBytes.
	QueueBytes int

	// RefusedQueueBytes is the same bound for Records of requests that never
	// reached an upstream. <= 0 → DefaultRefusedQueueBytes.
	RefusedQueueBytes int

	// Logger is used for drop / sink-error warnings. nil → slog.Default.
	Logger *slog.Logger
}

// Emitter fans captured Records to its Sinks via a single drain goroutine,
// which takes everything queued at once and hands it to each Sink as one
// batch. The queues are bounded by the bytes their Records hold, not by
// count: record sizes span three orders of magnitude, so a count sized for
// large bodies drops small ones in a burst. Drop-on-full preserves the
// "post-flight never blocks" invariant; the drop count is exposed via
// Dropped().
//
// Records of requests that never reached an upstream wait in a separate,
// smaller queue that the drain reads only while the main one is empty, so a
// flood of refusals drops refusal Records, never served ones.
type Emitter struct {
	served  recordQueue
	refused recordQueue
	ready   chan struct{}
	stop    chan struct{}
	sinks   []Sink
	log     *slog.Logger
	// spare is the drain's empty batch slice, swapped with a queue's.
	spare []Record

	wg             sync.WaitGroup
	stopped        atomic.Bool
	dropped        atomic.Uint64
	droppedRefused atomic.Uint64
}

// NewEmitter constructs an Emitter and starts its drain goroutine. Close
// it at shutdown to flush in-flight Records.
func NewEmitter(opts EmitterOptions, sinks ...Sink) *Emitter {
	qbytes := opts.QueueBytes
	if qbytes <= 0 {
		qbytes = DefaultQueueBytes
	}
	rbytes := opts.RefusedQueueBytes
	if rbytes <= 0 {
		rbytes = DefaultRefusedQueueBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	e := &Emitter{
		served:  recordQueue{max: qbytes},
		refused: recordQueue{max: rbytes},
		ready:   make(chan struct{}, 1),
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
	if !e.served.push(r) {
		e.recordDrop("payload", &e.dropped, e.served.max)
		return
	}
	e.wake()
}

// EmitRefused queues r, the Record of a request that never reached an
// upstream, on the refused queue. A full queue drops it, counted as
// payload_refused.
func (e *Emitter) EmitRefused(r Record) {
	if e.stopped.Load() {
		return
	}
	if !e.refused.push(r) {
		e.recordDrop("payload_refused", &e.droppedRefused, e.refused.max)
		return
	}
	e.wake()
}

func (e *Emitter) wake() {
	select {
	case e.ready <- struct{}{}:
	default:
	}
}

func (e *Emitter) recordDrop(kind string, counter *atomic.Uint64, maxBytes int) {
	metrics.RecordLost(kind)
	n := counter.Add(1)
	if n == 1 || n&(n-1) == 0 {
		e.log.Warn("payloadlog: queue full, record dropped",
			"queue", kind,
			"total_dropped", n,
			"queue_bytes", maxBytes,
		)
	}
}

// TryEmit queues r on the main queue like Emit but reports a full or closed queue instead of dropping: nothing is counted or logged, the caller decides what a refused Record means.
func (e *Emitter) TryEmit(r Record) bool {
	if e.stopped.Load() || !e.served.push(r) {
		return false
	}
	e.wake()
	return true
}

// Free returns how many more bytes the queue takes before Emit starts dropping. Concurrent emits make it a snapshot, not a reservation.
func (e *Emitter) Free() int { return e.served.max - e.served.held() }

// Capacity returns the queue's budget in bytes.
func (e *Emitter) Capacity() int { return e.served.max }

// Dropped returns the cumulative count of Records dropped due to a full
// queue.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// DroppedRefused returns the cumulative count of refused requests' Records
// dropped because the refused queue was full.
func (e *Emitter) DroppedRefused() uint64 { return e.droppedRefused.Load() }

// RefusedQueueDepth returns the number of refused requests' Records waiting.
func (e *Emitter) RefusedQueueDepth() int { return e.refused.depth() }

// QueueDepth returns the number of Records waiting in the bounded queue —
// the leading signal before Dropped starts counting. Safe to call
// concurrently.
func (e *Emitter) QueueDepth() int { return e.served.depth() }

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
		if e.writeQueued(&e.served) || e.writeQueued(&e.refused) {
			continue
		}
		select {
		case <-e.ready:
		case <-e.stop:
			// Flush what was queued before Close; later racing Emits are lost.
			for e.writeQueued(&e.served) {
			}
			for e.writeQueued(&e.refused) {
			}
			return
		}
	}
}

// writeQueued writes everything waiting in q as one batch and reports
// whether there was anything. The batch's bytes stay counted against q until
// the write returns, so the budget also bounds the batch being written.
func (e *Emitter) writeQueued(q *recordQueue) bool {
	batch := q.swap(e.spare)
	if len(batch) == 0 {
		e.spare = batch
		return false
	}
	e.write(batch)
	n := 0
	for i := range batch {
		n += recordBytes(batch[i])
	}
	clear(batch)
	q.release(n)
	e.spare = batch[:0]
	return true
}

func (e *Emitter) write(batch []Record) {
	for _, sink := range e.sinks {
		if bw, ok := sink.(BatchWriter); ok {
			if err := bw.WriteBatch(batch); err != nil {
				e.log.Warn("payloadlog: sink batch write failed",
					"err", err,
					"records", len(batch),
				)
			}
			continue
		}
		for i := range batch {
			if err := sink.Write(batch[i]); err != nil {
				e.log.Warn("payloadlog: sink write failed",
					"err", err,
					"request_id", batch[i].RequestID,
				)
			}
		}
	}
}
