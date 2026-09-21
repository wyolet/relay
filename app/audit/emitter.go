package audit

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/metrics"
)

// Dropped counts audit events discarded because the queue was full. A
// dropped audit row is a hole in the record, so it gets its own counter
// rather than a label on the shared records_lost_total.
var Dropped = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "audit",
	Name:      "dropped_total",
	Help:      "Audit events dropped because the emitter queue was full.",
})

func init() { metrics.Register(Dropped) }

const (
	// QueueSize is the bounded-channel capacity. Admin traffic is orders of
	// magnitude below the data plane; 4096 absorbs a long PG stall.
	QueueSize = 4096
	// batchSize and batchInterval bound one insert pass.
	batchSize     = 100
	batchInterval = time.Second
)

// pruneInterval is how often retention is enforced. A var so a test can
// reach the prune branch of the drain loop without waiting an hour.
var pruneInterval = time.Hour

// Sink persists a batch of events.
type Sink interface {
	Write(ctx context.Context, events []Event) error
	Prune(ctx context.Context, before time.Time) (int64, error)
}

// Emitter is the write path: handlers Emit, one goroutine batches into the
// Sink and enforces retention. Emit never blocks — a full queue drops and
// counts, which is the same contract the data plane's observers hold.
type Emitter struct {
	queue chan Event
	sink  Sink
	log   *slog.Logger

	wg      sync.WaitGroup
	stop    chan struct{}
	stopped atomic.Bool
	dropped atomic.Uint64

	// pruning keeps the ticker from starting a second prune over the same
	// rows while the first is still running: a prune of a large table takes
	// longer than the interval.
	pruning atomic.Bool
	// retentionDays is read live so a settings change lands on the next
	// prune without a restart. 0 disables pruning.
	retentionDays atomic.Int64
}

// NewEmitter starts the drain goroutine. Close it at shutdown.
func NewEmitter(sink Sink, log *slog.Logger) *Emitter {
	if log == nil {
		log = slog.Default()
	}
	e := &Emitter{
		queue: make(chan Event, QueueSize),
		sink:  sink,
		log:   log,
		stop:  make(chan struct{}),
	}
	e.wg.Add(1)
	go e.drain()
	return e
}

// SetRetentionDays updates the live retention window. Wired to the "audit"
// settings section by the composition root.
func (e *Emitter) SetRetentionDays(days int) { e.retentionDays.Store(int64(days)) }

// Emit queues ev. Non-blocking: a full queue drops and increments Dropped.
func (e *Emitter) Emit(ev Event) {
	if e == nil || e.stopped.Load() {
		return
	}
	if ev.ID == "" {
		ev.ID = ids.New()
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	select {
	case e.queue <- ev:
	default:
		Dropped.Inc()
		n := e.dropped.Add(1)
		// Warn once per power-of-2 to avoid log spam under sustained drop.
		if n == 1 || n&(n-1) == 0 {
			e.log.Warn("audit: queue full, event dropped", "total_dropped", n, "queue_size", cap(e.queue))
		}
	}
}

// DroppedCount is the cumulative drop count, for assertions.
func (e *Emitter) DroppedCount() uint64 { return e.dropped.Load() }

// QueueDepth is the number of events waiting to be written.
func (e *Emitter) QueueDepth() int { return len(e.queue) }

// Close drains what is queued and stops the prune loop. The queue channel is
// never closed: a concurrent Emit would panic on the send, and an audit trail
// must not be able to take the process down at shutdown.
func (e *Emitter) Close() {
	if e == nil || e.stopped.Swap(true) {
		return
	}
	close(e.stop)
	e.wg.Wait()
}

func (e *Emitter) drain() {
	defer e.wg.Done()
	flush := time.NewTicker(batchInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	batch := make([]Event, 0, batchSize)
	for {
		select {
		case <-e.stop:
			batch = e.drainQueued(batch)
			e.write(batch)
			return
		case ev := <-e.queue:
			batch = append(batch, ev)
			if len(batch) >= batchSize {
				e.write(batch)
				batch = batch[:0]
			}
		case <-flush.C:
			if len(batch) > 0 {
				e.write(batch)
				batch = batch[:0]
			}
		case <-prune.C:
			// Off the drain loop: a prune over a large table takes seconds,
			// and the queue fills (and drops) for as long as it runs.
			go e.Prune()
		}
	}
}

// drainQueued appends everything already queued without blocking, so a Close
// loses only what arrives after it.
func (e *Emitter) drainQueued(batch []Event) []Event {
	for {
		select {
		case ev := <-e.queue:
			batch = append(batch, ev)
			if len(batch) >= batchSize {
				e.write(batch)
				batch = batch[:0]
			}
		default:
			return batch
		}
	}
}

func (e *Emitter) write(batch []Event) {
	if len(batch) == 0 || e.sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := e.sink.Write(ctx, batch)
	if err != nil {
		// One immediate retry: a sink blip is the common failure and losing
		// the rows silently is the one outcome an audit log may not have.
		err = e.sink.Write(ctx, batch)
	}
	if err != nil {
		e.log.Warn("audit: batch write failed", "err", err, "rows", len(batch))
		Dropped.Add(float64(len(batch)))
		e.dropped.Add(uint64(len(batch)))
	}
}

// Prune enforces the current retention window. Exported so the prune pass
// is testable without waiting an hour.
func (e *Emitter) Prune() {
	days := e.retentionDays.Load()
	if days <= 0 || e.sink == nil {
		return
	}
	if !e.pruning.CompareAndSwap(false, true) {
		return
	}
	defer e.pruning.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	before := time.Now().UTC().AddDate(0, 0, -int(days))
	n, err := e.sink.Prune(ctx, before)
	if err != nil {
		e.log.Warn("audit: prune failed", "err", err)
		return
	}
	if n > 0 {
		e.log.Info("audit: pruned expired events", "rows", n, "before", before)
	}
}
