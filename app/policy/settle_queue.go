package policy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// SettleQueueConfig sizes the settle queue. A QueueSize of 0 leaves it off.
type SettleQueueConfig struct {
	// BatchSize is the most settles one round trip carries.
	BatchSize int
	// MaxDelay is how long the first settle of a batch waits for others; 0 sends what is already queued.
	MaxDelay time.Duration
	// QueueSize bounds the settles waiting for a writer; one that finds it full settles on its own round trip.
	QueueSize int
	// PoolSize is the post-flight store's connection pool size; 0 if it has none.
	PoolSize int
}

// settleFlushers is the number of batches in flight at once: a stall on one connection (a TCP retransmit) leaves the others draining the queue, and the post-flight pool keeps its other connections for host-health writes and the settles a full queue turns away.
const settleFlushers = 4

// settleBatchTimeout bounds one batch's round trip, as the post-flight deadline bounds a direct settle.
const settleBatchTimeout = 5 * time.Second

// settleDrainTimeout bounds how long CloseSettleQueue waits for the queued settles to land.
const settleDrainTimeout = 5 * time.Second

// WithSettleQueue coalesces Settle across requests: a collector groups queued settles into batches of up to cfg.BatchSize, sent once full or cfg.MaxDelay after their first settle, and settleFlushers writers (no more than cfg.PoolSize) send each batch as one pipelined round trip on the post-flight store. Settle still returns its own outcome. Call after WithPostFlight and before serving; CloseSettleQueue drains it.
func (s *Service) WithSettleQueue(cfg SettleQueueConfig, log *slog.Logger) *Service {
	if cfg.QueueSize <= 0 || s.postSelector == nil {
		return s
	}
	s.settleQueue = newSettleQueue(s, cfg, log)
	flushers := settleFlushers
	if cfg.PoolSize > 0 {
		// More batches in flight than connections would only wait on the pool.
		flushers = min(flushers, cfg.PoolSize)
	}
	s.settleQueue.start(flushers)
	return s
}

// CloseSettleQueue stops taking settles and waits, up to settleDrainTimeout, for the queued ones to land. Settle keeps working afterwards, on round trips of its own.
func (s *Service) CloseSettleQueue() {
	if s.settleQueue != nil {
		s.settleQueue.close(settleDrainTimeout)
	}
}

// settleQueue: Settle sends a queuedSettle on in and waits on its done; collect groups them into batches and the flushers run each batch.
type settleQueue struct {
	svc       *Service
	log       *slog.Logger
	batchSize int
	maxDelay  time.Duration

	// mu guards closed against a send racing the close of in.
	mu      sync.RWMutex
	closed  bool
	in      chan *queuedSettle
	batches chan []*queuedSettle
	done    sync.WaitGroup
}

type queuedSettle struct {
	// ctx is the caller's, for its log fields only: the batch runs under its own deadline.
	ctx     context.Context
	inbound *pkgratelimit.Reservation
	acq     *Acquisition
	obs     pkgratelimit.Observations
	result  chan error
}

var queuedSettles = sync.Pool{New: func() any { return &queuedSettle{result: make(chan error, 1)} }}

func newSettleQueue(svc *Service, cfg SettleQueueConfig, log *slog.Logger) *settleQueue {
	if log == nil {
		log = slog.Default()
	}
	return &settleQueue{
		svc:       svc,
		log:       log,
		batchSize: max(cfg.BatchSize, 1),
		maxDelay:  max(cfg.MaxDelay, 0),
		in:        make(chan *queuedSettle, cfg.QueueSize),
		batches:   make(chan []*queuedSettle),
	}
}

func (q *settleQueue) start(flushers int) {
	q.done.Add(1 + flushers)
	go q.collect()
	for range flushers {
		go q.flush()
	}
}

// settle queues one settle and waits for its outcome. queued is false when the queue is full or closed: the caller settles directly.
func (q *settleQueue) settle(ctx context.Context, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) (queued bool, err error) {
	it := queuedSettles.Get().(*queuedSettle)
	it.ctx, it.inbound, it.acq, it.obs = ctx, inbound, acq, obs
	if !q.enqueue(it) {
		it.reset()
		queuedSettles.Put(it)
		return false, nil
	}
	select {
	case err := <-it.result:
		it.reset()
		queuedSettles.Put(it)
		return true, err
	case <-ctx.Done():
		// The settle still lands; only its outcome goes unread, so it is not reused.
		return true, fmt.Errorf("policy: settle queued, outcome not awaited: %w", ctx.Err())
	}
}

func (q *settleQueue) enqueue(it *queuedSettle) bool {
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

func (it *queuedSettle) reset() {
	it.ctx, it.inbound, it.acq, it.obs = nil, nil, nil, pkgratelimit.Observations{}
}

// collect groups queued settles into batches until in is closed and drained.
func (q *settleQueue) collect() {
	defer q.done.Done()
	defer close(q.batches)
	var timer *time.Timer
	if q.maxDelay > 0 {
		timer = time.NewTimer(q.maxDelay)
		timer.Stop()
	}
	for first := range q.in {
		batch, open := q.fill(append(make([]*queuedSettle, 0, q.batchSize), first), timer)
		q.batches <- batch
		if !open {
			return
		}
	}
}

// fill adds queued settles to batch until it is full or, with a timer, maxDelay has passed; without one it takes only what is already queued. open is false once in is closed and drained.
func (q *settleQueue) fill(batch []*queuedSettle, timer *time.Timer) (_ []*queuedSettle, open bool) {
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

func (q *settleQueue) flush() {
	defer q.done.Done()
	for batch := range q.batches {
		q.run(batch)
	}
}

// run sends batch as one round trip and hands each settle its own outcome: a command that fails fails only the settle it belongs to. A settle whose commands failed in transport is resent once on its own round trip, which the commit guard makes safe when the first one ran; run returns once those land too, so a close still drains them.
func (q *settleQueue) run(batch []*queuedSettle) {
	ctx, cancel := context.WithTimeout(context.Background(), settleBatchTimeout)
	defer cancel()
	b := q.svc.postSelector.NewBatch()
	prepared := make([]preparedSettle, len(batch))
	for i, it := range batch {
		prepared[i] = q.svc.prepareSettle(b, it.inbound, it.acq, it.obs)
	}
	b.Exec(ctx)
	var retries sync.WaitGroup
	for i, it := range batch {
		// Each send is the last use of it: its caller reuses it once received.
		if prepared[i].transportFailed() {
			metrics.SettleRetries.Inc()
			retries.Go(func() { it.result <- q.retry(it) })
			continue
		}
		it.result <- prepared[i].finish(it.ctx, ctx)
	}
	retries.Wait()
}

// retry settles it directly under a post-flight deadline of its own, keeping the caller's log fields but not its cancellation.
func (q *settleQueue) retry(it *queuedSettle) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(it.ctx), settleBatchTimeout)
	defer cancel()
	return q.svc.settleNow(ctx, it.inbound, it.acq, it.obs)
}

// close stops intake and waits up to timeout for the queued settles to land.
func (q *settleQueue) close(timeout time.Duration) {
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
	case <-time.After(timeout):
		q.log.Warn("policy: settle queue closed before draining", "pending", len(q.in))
	}
}
