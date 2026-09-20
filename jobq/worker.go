package jobq

import (
	"context"
	"fmt"
	"time"
)

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
			if parent.Err() != nil {
				q.finalizeRequeue(job)
				return
			}
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
		if err := q.store.markCompleted(fctx, job.ID, job.Attempt, uri); err != nil {
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
		if err := q.store.markDiscarded(fctx, job.ID, job.Attempt, cause.Error()); err != nil {
			q.opts.Logger.Warn("jobq: mark discarded failed", "id", job.ID, "err", err)
		}
		return
	}
	next := time.Now().Add(q.opts.Backoff(job.Attempt))
	if err := q.store.markRetryable(fctx, job.ID, job.Attempt, next, cause.Error()); err != nil {
		q.opts.Logger.Warn("jobq: mark retryable failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeDiscard(job *Job, msg string) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markDiscarded(fctx, job.ID, job.Attempt, msg); err != nil {
		q.opts.Logger.Warn("jobq: mark discarded failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeCancel(job *Job) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markCancelledRunning(fctx, job.ID, job.Attempt, "jobq: cancelled by caller"); err != nil {
		q.opts.Logger.Warn("jobq: mark cancelled failed", "id", job.ID, "err", err)
	}
}

func (q *Queue) finalizeRequeue(job *Job) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.store.markRequeue(fctx, job.ID, job.Attempt); err != nil {
		q.opts.Logger.Warn("jobq: requeue failed", "id", job.ID, "err", err)
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
