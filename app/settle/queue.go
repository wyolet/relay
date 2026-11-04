// Package settle coalesces the post-flight writes of many served requests into shared round trips on the post-flight store: a collector groups queued settles into batches, sent once full or a short delay after their first, and a few writers send each batch as one pipelined round trip.
//
// Scope: queueing, batching, the full and closed queue fallbacks, resending a settle whose shared round trip failed in transport, and draining at close. Out of scope: what a settle writes; the limiter's commits and the key pool's success write queue themselves on the batch.
//
// Expected kv ops per request: none of its own. A settle's writes ride a shared pipelined round trip, or one of their own when the queue is full or closed; a settle with nothing to write makes none and does not wait. One whose shared round trip failed in transport is sent once more on its own.
package settle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

// Writes is one settle's commands queued on a batch.
type Writes interface {
	// TransportFailed reports whether a command failed in transport rather than with its own error reply: whether it ran is unknown.
	TransportFailed() bool
	// Finish reads the outcome once the batch has run.
	Finish(ctx context.Context)
	// Resend writes the settle again on a round trip of its own; the commit guard makes a commit that already ran a no-op.
	Resend(ctx context.Context)
}

// Config sizes the queue. A QueueSize of 0 leaves it off: every settle goes out on a round trip of its own.
type Config struct {
	// BatchSize is the most settles one round trip carries.
	BatchSize int
	// MaxDelay is how long the first settle of a batch waits for others; 0 sends what is already queued.
	MaxDelay time.Duration
	// QueueSize bounds the settles waiting for a writer; one that finds it full goes out on its own round trip.
	QueueSize int
	// PoolSize is the post-flight store's connection pool size; 0 if it has none.
	PoolSize int
}

const (
	// writers is the number of batches in flight at once: a stall on one connection (a TCP retransmit) leaves the others draining the queue, and the pool keeps its other connections for host-health writes and the settles a full queue turns away.
	writers = 4
	// batchTimeout bounds one batch's round trip, as the post-flight deadline bounds a direct settle.
	batchTimeout = 5 * time.Second
	// drainTimeout bounds how long Close waits for the queued settles to land.
	drainTimeout = 5 * time.Second
	// settleOps sizes one settle's batch: two commits and a success write.
	settleOps = 4
)

// Queue coalesces settles. Concurrent calls are safe.
type Queue struct {
	store     kv.Store
	log       *slog.Logger
	batchSize int
	maxDelay  time.Duration
	writers   int

	// mu guards closed against a send racing the close of in.
	mu      sync.RWMutex
	closed  bool
	in      chan *queued
	batches chan []*queued
	done    sync.WaitGroup
}

type queued struct {
	// ctx is the caller's, for its log fields only: the batch runs under its own deadline.
	ctx    context.Context
	b      *kv.Batch
	w      Writes
	landed chan struct{}
}

var queuedPool = sync.Pool{New: func() any { return &queued{landed: make(chan struct{}, 1)} }}

func (it *queued) reset() { it.ctx, it.b, it.w = nil, nil, nil }

// New returns a queue sending batches on store, or nil when cfg.QueueSize is 0. Call Start before serving and Close at shutdown.
func New(store kv.Store, cfg Config, log *slog.Logger) *Queue {
	if cfg.QueueSize <= 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	n := writers
	if cfg.PoolSize > 0 {
		// More batches in flight than connections would only wait on the pool.
		n = min(n, cfg.PoolSize)
	}
	return &Queue{
		store:     store,
		log:       log,
		batchSize: max(cfg.BatchSize, 1),
		maxDelay:  max(cfg.MaxDelay, 0),
		writers:   n,
		in:        make(chan *queued, cfg.QueueSize),
		batches:   make(chan []*queued),
	}
}

// Start runs the collector and the writers until Close.
func (q *Queue) Start() {
	q.done.Add(1 + q.writers)
	go q.collect()
	for range q.writers {
		go q.write()
	}
}

// NewBatch returns a batch for one settle's commands, on the store the queue sends to.
func (q *Queue) NewBatch() *kv.Batch { return kv.NewBatch(q.store, settleOps) }

// Settle sends the commands w queued on b, a batch from NewBatch, in a shared round trip and returns once they landed. A full or closed queue sends them on a round trip of their own; a settle that queued nothing returns at once. A caller that leaves first does not lose the settle: it still lands.
func (q *Queue) Settle(ctx context.Context, b *kv.Batch, w Writes) {
	if b.Len() == 0 {
		w.Finish(ctx)
		return
	}
	it := queuedPool.Get().(*queued)
	it.ctx, it.b, it.w = ctx, b, w
	if !q.enqueue(it) {
		it.reset()
		queuedPool.Put(it)
		b.Exec(ctx)
		w.Finish(ctx)
		return
	}
	select {
	case <-it.landed:
		it.reset()
		queuedPool.Put(it)
	case <-ctx.Done():
		// The settle still lands; only its signal goes unread, so it is not reused.
	}
}

func (q *Queue) enqueue(it *queued) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return false
	}
	select {
	case q.in <- it:
		return true
	default:
		metrics.SettleQueueFull.Inc()
		return false
	}
}

// collect groups queued settles into batches until in is closed and drained.
func (q *Queue) collect() {
	defer q.done.Done()
	defer close(q.batches)
	var timer *time.Timer
	if q.maxDelay > 0 {
		timer = time.NewTimer(q.maxDelay)
		timer.Stop()
	}
	for first := range q.in {
		batch, open := q.fill(append(make([]*queued, 0, q.batchSize), first), timer)
		q.batches <- batch
		if !open {
			return
		}
	}
}

// fill adds queued settles to batch until it is full or, with a timer, maxDelay has passed; without one it takes only what is already queued. open is false once in is closed and drained.
func (q *Queue) fill(batch []*queued, timer *time.Timer) (_ []*queued, open bool) {
	var expired <-chan time.Time
	if timer != nil {
		timer.Reset(q.maxDelay)
		defer timer.Stop()
		expired = timer.C
	}
	for len(batch) < q.batchSize {
		if expired == nil {
			select {
			case it, ok := <-q.in:
				if !ok {
					return batch, false
				}
				batch = append(batch, it)
			default:
				return batch, true
			}
			continue
		}
		select {
		case it, ok := <-q.in:
			if !ok {
				return batch, false
			}
			batch = append(batch, it)
		case <-expired:
			return batch, true
		}
	}
	return batch, true
}

func (q *Queue) write() {
	defer q.done.Done()
	for batch := range q.batches {
		q.run(batch)
	}
}

// run sends batch as one round trip and finishes each settle on its own outcome: a command that fails fails only its settle. A settle whose commands failed in transport is resent once on its own round trip; run returns once those land too, so a close still drains them.
func (q *Queue) run(batch []*queued) {
	ctx, cancel := context.WithTimeout(context.Background(), batchTimeout)
	defer cancel()
	// Each settle's results live in its own batch; this one carries only the commands.
	b := kv.NewBatch(q.store, 0)
	for _, it := range batch {
		b.Append(it.b)
	}
	b.Exec(ctx)
	var resends sync.WaitGroup
	for _, it := range batch {
		// Each signal is the last use of it: its caller reuses it once received.
		w, logCtx := it.w, it.ctx
		if w.TransportFailed() {
			metrics.SettleRetries.Inc()
			resends.Go(func() {
				q.resend(logCtx, w)
				it.landed <- struct{}{}
			})
			continue
		}
		w.Finish(logCtx)
		it.landed <- struct{}{}
	}
	resends.Wait()
}

// resend writes w again under a post-flight deadline of its own, keeping the caller's log fields but not its cancellation.
func (q *Queue) resend(logCtx context.Context, w Writes) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(logCtx), batchTimeout)
	defer cancel()
	w.Resend(ctx)
}

// Close stops taking settles and waits, up to drainTimeout, for the queued ones to land. Settle keeps working afterwards, on round trips of its own.
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.in)
	q.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		q.done.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		q.log.Warn("settle: queue closed before draining", "pending", len(q.in))
	}
}

// TransportError reports a command failure after which whether the command ran is unknown: the store took it and never answered, or the deadline ran out.
func TransportError(err error) bool {
	return errors.Is(err, kv.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
