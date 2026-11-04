package settle

// Queued is the number of settles waiting in the channel, for tests that hold the writers back.
func (q *Queue) Queued() int { return len(q.in) }
