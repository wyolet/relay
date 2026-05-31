//go:build integration

package jobq

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/jobq/payload"
)

// fastOpts shrinks every interval so integration tests run in well under a
// second while still exercising the real loops.
func fastOpts() Options {
	return Options{
		Concurrency:      8,
		PollInterval:     10 * time.Millisecond,
		ScheduleInterval: 10 * time.Millisecond,
		RescueInterval:   10 * time.Millisecond,
		RescueAfter:      time.Hour, // disable auto-rescue except where tested directly
		JobTimeout:       5 * time.Second,
		Backoff:          func(int) time.Duration { return 10 * time.Millisecond },
	}
}

func testQueue(t *testing.T, opts Options) (*Queue, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE jobq_jobs"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	ps, err := payload.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("payload store: %v", err)
	}
	return New(pool, ps, opts), pool
}

// run starts the queue under a cancellable context and tears it down cleanly.
func run(t *testing.T, q *Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cancel(); q.Wait() })
}

func waitState(t *testing.T, q *Queue, id string, want State, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last State
	for time.Now().Before(deadline) {
		j, err := q.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		last = j.State
		if j.State == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s (last %s)", id, want, last)
	return nil
}

func countState(t *testing.T, pool *pgxpool.Pool, state State) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM jobq_jobs WHERE state = $1", string(state)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestIntegration_EnqueueRunComplete(t *testing.T) {
	q, _ := testQueue(t, fastOpts())
	q.Register("echo", func(_ context.Context, j *Job) ([]byte, error) {
		return append([]byte("out:"), j.Input()...), nil
	})
	run(t, q)

	id, err := q.Enqueue(context.Background(), []byte("ping"), EnqueueOpts{Queue: "echo"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	j := waitState(t, q, id, StateCompleted, 3*time.Second)
	// "PG never holds bytes": the row carries only file: URIs, not payload.
	if j.InputURI == "" || j.ResultURI == "" {
		t.Fatalf("expected input/result URIs, got input=%q result=%q", j.InputURI, j.ResultURI)
	}
	if j.FinalizedAt == nil {
		t.Fatal("completed job has nil finalized_at")
	}

	out, err := q.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(out) != "out:ping" {
		t.Fatalf("result = %q, want %q", out, "out:ping")
	}
}

func TestIntegration_ClaimRaceSafety(t *testing.T) {
	q, pool := testQueue(t, fastOpts())

	const n = 50
	var runs sync.Map // id -> *int32
	var total int32
	q.Register("work", func(_ context.Context, j *Job) ([]byte, error) {
		v, _ := runs.LoadOrStore(j.ID, new(int32))
		atomic.AddInt32(v.(*int32), 1)
		atomic.AddInt32(&total, 1)
		return nil, nil
	})
	run(t, q)

	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(context.Background(), nil, EnqueueOpts{Queue: "work"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for countState(t, pool, StateCompleted) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d completed", countState(t, pool, StateCompleted), n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&total); got != n {
		t.Fatalf("total runs = %d, want %d", got, n)
	}
	runs.Range(func(_, v any) bool {
		if c := atomic.LoadInt32(v.(*int32)); c != 1 {
			t.Errorf("a job ran %d times, want exactly 1", c)
		}
		return true
	})
}

func TestIntegration_RetryThenDiscard(t *testing.T) {
	q, _ := testQueue(t, fastOpts())
	var attempts int32
	q.Register("flaky", func(_ context.Context, _ *Job) ([]byte, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, fmt.Errorf("always fails")
	})
	run(t, q)

	id, err := q.Enqueue(context.Background(), nil, EnqueueOpts{Queue: "flaky", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	j := waitState(t, q, id, StateDiscarded, 5*time.Second)
	if j.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", j.Attempt)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("handler ran %d times, want 3", got)
	}
	if j.LastError == "" {
		t.Fatal("discarded job has empty last_error")
	}
}

func TestIntegration_RescueStuck(t *testing.T) {
	q, pool := testQueue(t, fastOpts())
	ctx := context.Background()

	// A stuck running job with attempts remaining → rescued to retryable.
	id, err := q.Enqueue(ctx, nil, EnqueueOpts{Queue: "x", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobq_jobs SET state='running', attempt=1, attempted_at=NOW()-INTERVAL '1 hour'
		WHERE id=$1`, id); err != nil {
		t.Fatalf("stick: %v", err)
	}

	// A stuck running job with attempts exhausted → rescued to discarded.
	id2, err := q.Enqueue(ctx, nil, EnqueueOpts{Queue: "x", MaxAttempts: 1})
	if err != nil {
		t.Fatalf("enqueue2: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobq_jobs SET state='running', attempt=1, attempted_at=NOW()-INTERVAL '1 hour'
		WHERE id=$1`, id2); err != nil {
		t.Fatalf("stick2: %v", err)
	}

	n, err := q.store.rescueStuck(ctx, time.Now().Add(-time.Minute), 100)
	if err != nil {
		t.Fatalf("rescue: %v", err)
	}
	if n != 2 {
		t.Fatalf("rescued %d, want 2", n)
	}

	if j, _ := q.Get(ctx, id); j.State != StateRetryable {
		t.Fatalf("job1 state = %s, want retryable", j.State)
	}
	if j, _ := q.Get(ctx, id2); j.State != StateDiscarded {
		t.Fatalf("job2 state = %s, want discarded", j.State)
	}
}

func TestIntegration_CancelPending(t *testing.T) {
	q, _ := testQueue(t, fastOpts())
	ctx := context.Background()
	// Schedule far in the future so it never runs before we cancel.
	id, err := q.Enqueue(ctx, nil, EnqueueOpts{Queue: "x", ScheduledAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	res, err := q.Cancel(ctx, id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res != CancelledPending {
		t.Fatalf("cancel result = %v, want CancelledPending", res)
	}
	if j, _ := q.Get(ctx, id); j.State != StateCancelled || j.FinalizedAt == nil {
		t.Fatalf("state = %s finalized=%v, want cancelled+finalized", j.State, j.FinalizedAt)
	}
}

func TestIntegration_CancelRunning(t *testing.T) {
	q, _ := testQueue(t, fastOpts())
	started := make(chan struct{})
	q.Register("block", func(ctx context.Context, _ *Job) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	run(t, q)

	id, err := q.Enqueue(context.Background(), nil, EnqueueOpts{Queue: "block"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}

	res, err := q.Cancel(context.Background(), id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res != CancelRequestedRunning {
		t.Fatalf("cancel result = %v, want CancelRequestedRunning", res)
	}
	waitState(t, q, id, StateCancelled, 3*time.Second)
}

func TestIntegration_ScheduledPromotion(t *testing.T) {
	q, _ := testQueue(t, fastOpts())
	var ran int32
	q.Register("later", func(_ context.Context, _ *Job) ([]byte, error) {
		atomic.AddInt32(&ran, 1)
		return nil, nil
	})
	run(t, q)

	id, err := q.Enqueue(context.Background(), nil, EnqueueOpts{
		Queue:       "later",
		ScheduledAt: time.Now().Add(150 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Initially scheduled, not available.
	if j, _ := q.Get(context.Background(), id); j.State != StateScheduled {
		t.Fatalf("initial state = %s, want scheduled", j.State)
	}
	waitState(t, q, id, StateCompleted, 3*time.Second)
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("ran %d times, want 1", ran)
	}
}
