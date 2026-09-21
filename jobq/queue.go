package jobq

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

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
