package jobq

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

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
		if inputURI != "" {
			if derr := q.payload.Delete(ctx, inputURI); derr != nil {
				q.opts.Logger.Warn("jobq: delete orphaned input failed", "id", id, "uri", inputURI, "err", derr)
			}
		}
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

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("jobq: generate id: %w", err)
	}
	return id.String(), nil
}
