package settle

import "time"

// Queued is the number of settles waiting in the channel, for tests that hold the writers back.
func (q *Queue) Queued() int { return len(q.in) }

// DrainTimeout is how long Close waits for pending settles.
func (q *Queue) DrainTimeout(pending int64) time.Duration { return q.drainTimeout(pending) }
