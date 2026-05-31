package jobq

import (
	"log/slog"
	"math/rand"
	"time"
)

// Options configures a Queue. The zero value is usable; New fills sane
// defaults for any unset field.
type Options struct {
	// Concurrency is the initial global cap on jobs running at once in this
	// process (the resizable gate). Adjustable at runtime via SetConcurrency.
	// Default: 8.
	Concurrency int

	// PollInterval is how long a worker waits before re-checking for work when
	// a queue is empty. Default: 1s. (A pg_notify wakeup would shorten this;
	// not wired yet.)
	PollInterval time.Duration

	// ScheduleInterval is how often the scheduler promotes due scheduled/
	// retryable jobs to available. Default: 1s.
	ScheduleInterval time.Duration

	// RescueInterval is how often the rescuer scans for stuck running jobs.
	// Default: 30s.
	RescueInterval time.Duration

	// RescueAfter is how long a job may sit in 'running' before the rescuer
	// presumes its worker died and reclaims it. MUST exceed JobTimeout so the
	// rescuer never reclaims a job a live worker is still running. Default: 15m.
	RescueAfter time.Duration

	// JobTimeout caps a single handler invocation; on expiry the job's context
	// is cancelled and the run counts as a failure. Default: 5m.
	JobTimeout time.Duration

	// Backoff computes the delay before a failed job's next attempt, given the
	// attempt number just completed (1-based). Default: exponential with
	// jitter, capped at 1h.
	Backoff func(attempt int) time.Duration

	// Logger receives operational warnings (handler panics, store errors in
	// maintenance loops). Default: slog.Default().
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Concurrency < 1 {
		o.Concurrency = 8
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.ScheduleInterval <= 0 {
		o.ScheduleInterval = time.Second
	}
	if o.RescueInterval <= 0 {
		o.RescueInterval = 30 * time.Second
	}
	if o.RescueAfter <= 0 {
		o.RescueAfter = 15 * time.Minute
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = 5 * time.Minute
	}
	if o.Backoff == nil {
		o.Backoff = defaultBackoff
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// defaultBackoff is exponential (base 1s, doubling per attempt) with up to 20%
// jitter, capped at 1h. attempt is 1-based.
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 12 { // cap the shift so 1s<<shift can't overflow
		shift = 12
	}
	d := time.Second << uint(shift)
	if d > time.Hour {
		d = time.Hour
	}
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1))
	return d + jitter
}
