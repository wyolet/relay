package jobq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/jobq/payload"
)

// Queue is the jobq engine. One process constructs it once and uses it as a
// producer (Enqueue/Get/Result/Cancel), a consumer (Register + Start), or both.
// Producer-only callers never call Start; consumer callers register handlers
// then Start the worker pool and maintenance loops. Separating the two roles
// across processes (a future worker fleet) is just two Queues over one database.
type Queue struct {
	store   *store
	payload payload.Store
	opts    Options
	gate    *gate

	mu       sync.Mutex
	handlers map[string]Handler
	running  map[string]*runningJob
	started  bool

	wg sync.WaitGroup
}

type runningJob struct {
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// New constructs a Queue over pool and ps. opts may be the zero value.
func New(pool *pgxpool.Pool, ps payload.Store, opts Options) *Queue {
	o := opts.withDefaults()
	return &Queue{
		store:    &store{pool: pool},
		payload:  ps,
		opts:     o,
		gate:     newGate(o.Concurrency),
		handlers: map[string]Handler{},
		running:  map[string]*runningJob{},
	}
}

// ---- Producer surface ----

// Enqueue stores input via the PayloadStore and inserts a job. It returns the
// new job's id. A nil input enqueues a job with no input payload.
func (q *Queue) Enqueue(ctx context.Context, input []byte, opts EnqueueOpts) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}

	var inputURI string
	if input != nil {
		inputURI, err = q.payload.Put(ctx, id+"/input", input)
		if err != nil {
			return "", fmt.Errorf("jobq: store input: %w", err)
		}
	}

	now := time.Now()
	sched := opts.ScheduledAt
	state := StateAvailable
	if sched.IsZero() {
		sched = now
	} else if sched.After(now) {
		state = StateScheduled
	}

	j := &Job{
		ID:          id,
		Queue:       opts.queue(),
		State:       state,
		Priority:    opts.Priority,
		MaxAttempts: opts.maxAttempts(),
		InputURI:    inputURI,
		Metadata:    opts.Metadata,
		ScheduledAt: sched,
	}
	if err := q.store.insert(ctx, j); err != nil {
		return "", err
	}
	return id, nil
}

// Get returns the current job record (without its payload bytes).
func (q *Queue) Get(ctx context.Context, id string) (*Job, error) {
	return q.store.get(ctx, id)
}

// Result returns the job's output bytes. It errors with ErrNotCompleted unless
// the job finished successfully.
func (q *Queue) Result(ctx context.Context, id string) ([]byte, error) {
	j, err := q.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.State != StateCompleted || j.ResultURI == "" {
		return nil, ErrNotCompleted
	}
	return q.payload.Get(ctx, j.ResultURI)
}

// Cancel attempts to cancel a job. A not-yet-running job is moved to cancelled
// (CancelledPending). A job running in THIS process has its context cancelled
// and finalizes as cancelled (CancelRequestedRunning). A job running in another
// process, or already terminal/unknown, yields CancelNoop — cross-process
// cancellation of a running job is not yet supported.
func (q *Queue) Cancel(ctx context.Context, id string) (CancelResult, error) {
	ok, err := q.store.cancelPending(ctx, id)
	if err != nil {
		return CancelNoop, err
	}
	if ok {
		return CancelledPending, nil
	}
	q.mu.Lock()
	rj := q.running[id]
	q.mu.Unlock()
	if rj != nil {
		rj.cancelled.Store(true)
		rj.cancel()
		return CancelRequestedRunning, nil
	}
	return CancelNoop, nil
}

// ---- Consumer surface ----

// Register binds a Handler to a queue. Call before Start. Re-registering a
// queue replaces its handler.
func (q *Queue) Register(queue string, h Handler) {
	if queue == "" {
		queue = defaultQueue
	}
	q.mu.Lock()
	q.handlers[queue] = h
	q.mu.Unlock()
}

// SetConcurrency resizes the global concurrency gate at runtime. Raising it
// admits queued work immediately; lowering it lets in-flight jobs drain.
func (q *Queue) SetConcurrency(n int) { q.gate.resize(n) }

// Concurrency reports the current in-use count and limit.
func (q *Queue) Concurrency() (inUse, limit int) { return q.gate.stats() }

// Start launches a dispatcher per registered queue plus the scheduler and
// rescuer. It returns immediately; cancel ctx to stop. Use Wait to block until
// every background goroutine has exited.
func (q *Queue) Start(ctx context.Context) error {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return errors.New("jobq: already started")
	}
	queues := make([]string, 0, len(q.handlers))
	for name := range q.handlers {
		queues = append(queues, name)
	}
	if len(queues) == 0 {
		q.mu.Unlock()
		return errors.New("jobq: no handlers registered")
	}
	q.started = true
	q.mu.Unlock()

	for _, name := range queues {
		q.wg.Add(1)
		go func(qn string) { defer q.wg.Done(); q.dispatch(ctx, qn) }(name)
	}
	q.wg.Add(2)
	go func() { defer q.wg.Done(); q.scheduleLoop(ctx) }()
	go func() { defer q.wg.Done(); q.rescueLoop(ctx) }()
	return nil
}

// Wait blocks until all goroutines started by Start have stopped.
func (q *Queue) Wait() { q.wg.Wait() }

// ---- Internals ----

// dispatch is the pull loop for one queue: reserve a gate slot, then claim one
// job to fill it. This ordering guarantees every running job holds a slot, so
// the gate is a hard concurrency bound.
func (q *Queue) dispatch(ctx context.Context, queue string) {
	for {
		if err := q.gate.acquire(ctx); err != nil {
			return
		}
		job, err := q.store.claimOne(ctx, queue)
		if err != nil {
			q.gate.release()
			if ctx.Err() != nil {
				return
			}
			q.opts.Logger.Warn("jobq: claim failed", "queue", queue, "err", err)
			if !sleep(ctx, q.opts.PollInterval) {
				return
			}
			continue
		}
		if job == nil {
			q.gate.release()
			if !sleep(ctx, q.opts.PollInterval) {
				return
			}
			continue
		}
		q.wg.Add(1)
		go func(j *Job) {
			defer q.wg.Done()
			defer q.gate.release()
			q.execute(ctx, j)
		}(job)
	}
}

func (q *Queue) execute(parent context.Context, job *Job) {
	q.mu.Lock()
	h := q.handlers[job.Queue]
	q.mu.Unlock()
	if h == nil {
		// Claimed by a dispatcher whose handler vanished — a config error, not
		// a transient failure, so discard rather than retry.
		q.finalizeDiscard(job, "jobq: no handler for queue "+job.Queue)
		return
	}

	if job.InputURI != "" {
		in, err := q.payload.Get(parent, job.InputURI)
		if err != nil {
			q.finalizeFailure(job, fmt.Errorf("load input: %w", err))
			return
		}
		job.input = in
	}

	jctx, cancel := context.WithTimeout(parent, q.opts.JobTimeout)
	defer cancel()
	rj := &runningJob{cancel: cancel}
	q.trackRunning(job.ID, rj)
	defer q.untrackRunning(job.ID)

	out, err := safeRun(h, jctx, job)

	switch {
	case err == nil:
		// Success wins even if a shutdown is in progress — persist with a
		// detached context so the result is never lost.
		fctx, fcancel := finalizeCtx()
		defer fcancel()
		uri, perr := q.payload.Put(fctx, job.ID+"/result", out)
		if perr != nil {
			q.finalizeFailure(job, fmt.Errorf("store result: %w", perr))
			return
		}
		if err := q.store.markCompleted(fctx, job.ID, uri); err != nil {
			q.opts.Logger.Warn("jobq: mark completed failed", "id", job.ID, "err", err)
		}
	case rj.cancelled.Load():
		q.finalizeCancel(job)
	case parent.Err() != nil:
		// Graceful shutdown mid-job (not a real failure, not a timeout):
		// return to available without consuming the attempt.
		q.finalizeRequeue(job)
	default:
		q.finalizeFailure(job, err)
	}
}

func (q *Queue) finalizeFailure(job *Job, cause error) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if job.Attempt >= job.MaxAttempts {
		if err := q.store.markDiscarded(fctx, job.ID, cause.Error()); err != nil {
			q.opts.Logger.Warn("jobq: mark discarded failed", "id", job.ID, "err", err)
		}
		return
	}
	next := time.Now().Add(q.opts.Backoff(job.Attempt))
	if err := q.store.markRetryable(fctx, job.ID, next, cause.Error()); err != nil {
		q.opts.Logger.Warn("jobq: mark retryable failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeDiscard(job *Job, msg string) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markDiscarded(fctx, job.ID, msg); err != nil {
		q.opts.Logger.Warn("jobq: mark discarded failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeCancel(job *Job) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markCancelledRunning(fctx, job.ID, "jobq: cancelled by caller"); err != nil {
		q.opts.Logger.Warn("jobq: mark cancelled failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeRequeue(job *Job) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markRequeue(fctx, job.ID); err != nil {
		q.opts.Logger.Warn("jobq: requeue failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(q.opts.ScheduleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := q.store.promoteDue(fctx); err != nil {
				q.opts.Logger.Warn("jobq: promote failed", "err", err)
			}
			cancel()
		}
	}
}

func (q *Queue) rescueLoop(ctx context.Context) {
	t := time.NewTicker(q.opts.RescueInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			horizon := time.Now().Add(-q.opts.RescueAfter)
			if _, err := q.store.rescueStuck(fctx, horizon, 100); err != nil {
				q.opts.Logger.Warn("jobq: rescue failed", "err", err)
			}
			cancel()
		}
	}
}

func (q *Queue) trackRunning(id string, rj *runningJob) {
	q.mu.Lock()
	q.running[id] = rj
	q.mu.Unlock()
}

func (q *Queue) untrackRunning(id string) {
	q.mu.Lock()
	delete(q.running, id)
	q.mu.Unlock()
}

// safeRun invokes a handler, converting a panic into an error so one bad job
// can't take down a worker goroutine.
func safeRun(h Handler, ctx context.Context, job *Job) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jobq: handler panic: %v", r)
		}
	}()
	return h(ctx, job)
}

// finalizeCtx returns a context detached from the request/shutdown lifecycle so
// terminal state writes survive a worker being told to stop.
func finalizeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("jobq: generate id: %w", err)
	}
	return id.String(), nil
}
