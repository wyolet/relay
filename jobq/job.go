package jobq

import (
	"context"
	"errors"
	"time"
)

// State is a job's position in its lifecycle.
//
//	available → running → completed          (handler returned output)
//	                    → retryable           (failed, attempts remain; retried after ScheduledAt)
//	                    → discarded           (failed, attempts exhausted)
//	                    → cancelled           (caller cancelled)
//	scheduled → available                     (scheduler promotes when due)
//	retryable → available                     (scheduler promotes when due)
//
// completed, discarded, and cancelled are terminal: a job in a terminal state
// has FinalizedAt set, and nothing else does (enforced by a CHECK constraint).
type State string

const (
	StateAvailable State = "available"
	StateRunning   State = "running"
	StateScheduled State = "scheduled"
	StateRetryable State = "retryable"
	StateCompleted State = "completed"
	StateDiscarded State = "discarded"
	StateCancelled State = "cancelled"
)

// Terminal reports whether the state is final (no further transitions).
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateDiscarded, StateCancelled:
		return true
	default:
		return false
	}
}

// Job is the durable record of one unit of work. Callers read it via Get; the
// runner hands it to a Handler. The input payload is loaded lazily (from the
// PayloadStore) only when a handler is about to run it — Get does not fetch
// bytes.
type Job struct {
	ID          string
	Queue       string
	State       State
	Priority    int
	Attempt     int // number of times claimed for execution (1 on first run)
	MaxAttempts int
	InputURI    string
	ResultURI   string
	Metadata    map[string]string
	LastError   string
	ScheduledAt time.Time
	AttemptedAt *time.Time // nil until first claim
	FinalizedAt *time.Time // non-nil iff State.Terminal()
	CreatedAt   time.Time

	input []byte // populated for the Handler only; nil otherwise
}

// Input returns the job's input payload. It is populated only inside a Handler
// invocation; it is nil when a Job is obtained via Get.
func (j *Job) Input() []byte { return j.input }

// Meta returns the metadata value for key, or "" if absent. Metadata is opaque
// to jobq — consumers use it to correlate jobs (e.g. batch_id, item_idx).
func (j *Job) Meta(key string) string { return j.Metadata[key] }

// Handler runs one job: input bytes in, output bytes out. The returned output
// is persisted by the runner to the PayloadStore and referenced by ResultURI.
// A non-nil error triggers retry (if attempts remain) or discard. Handlers
// must honour ctx cancellation — it fires on caller cancel or job timeout.
//
// Handlers must be safe to call concurrently and idempotent where possible: a
// worker crash can cause a job to run more than once (at-least-once delivery).
type Handler func(ctx context.Context, job *Job) (output []byte, err error)

// EnqueueOpts configures a single Enqueue. The zero value enqueues to the
// "default" queue, runnable immediately, with MaxAttempts 1.
type EnqueueOpts struct {
	Queue       string            // target queue (handler selector); "" → "default"
	MaxAttempts int               // total attempts before discard; <1 → 1
	Priority    int               // lower runs first; default 0
	Metadata    map[string]string // opaque correlation data
	ScheduledAt time.Time         // earliest run time; zero → now
}

const defaultQueue = "default"

func (o EnqueueOpts) queue() string {
	if o.Queue == "" {
		return defaultQueue
	}
	return o.Queue
}

func (o EnqueueOpts) maxAttempts() int {
	if o.MaxAttempts < 1 {
		return 1
	}
	return o.MaxAttempts
}

// Errors returned by the client surface.
var (
	// ErrNotFound is returned by Get/Result/Cancel for an unknown id.
	ErrNotFound = errors.New("jobq: job not found")
	// ErrNotCompleted is returned by Result when the job has no result yet
	// (still pending, running, or terminated without success).
	ErrNotCompleted = errors.New("jobq: job has no result")
)

// CancelResult reports the outcome of a Cancel call.
type CancelResult int

const (
	// CancelNoop means the job was already terminal or unknown to this node.
	CancelNoop CancelResult = iota
	// CancelledPending means a not-yet-running job was moved to cancelled.
	CancelledPending
	// CancelRequestedRunning means the job is running in THIS process and its
	// context was cancelled; it finalizes as cancelled when the handler returns.
	// Running jobs owned by another process are not reached (see Cancel docs).
	CancelRequestedRunning
)
