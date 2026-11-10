package catalogserver

import (
	"context"
	"sync"
	"time"
)

// catchUpWait bounds how long a request for a tag this replica doesn't hold
// yet waits for an early sync before it gets its 404.
const catchUpWait = 20 * time.Second

// syncRequests coalesces requests for an early sync: one is signalled at
// most every gap, and each request is answered by the first sync that starts
// after it. A request inside the gap is signalled when the gap ends rather
// than dropped, so the last of a burst of pushes is never missed.
type syncRequests struct {
	mu        sync.Mutex
	gap       time.Duration
	next      chan struct{} // closed when the next sync to start has finished
	last      time.Time
	scheduled bool
}

// requestSync asks the Run loop for an early sync and returns a channel
// closed once a sync that started after this call has finished.
func (s *Server) requestSync() <-chan struct{} {
	q := &s.syncReqs
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.next == nil {
		q.next = make(chan struct{})
	}
	next := q.next
	if q.scheduled {
		return next
	}
	if wait := q.gap - time.Since(q.last); wait > 0 {
		q.scheduled = true
		time.AfterFunc(wait, s.signalScheduledSync)
		return next
	}
	q.last = time.Now()
	s.signalSync()
	return next
}

// signalScheduledSync skips the signal when a regular sync already answered
// every request made during the gap.
func (s *Server) signalScheduledSync() {
	q := &s.syncReqs
	q.mu.Lock()
	q.scheduled = false
	waiting := q.next != nil
	if waiting {
		q.last = time.Now()
	}
	q.mu.Unlock()
	if waiting {
		s.signalSync()
	}
}

func (s *Server) signalSync() {
	select {
	case s.fetchNow <- struct{}{}:
	default:
	}
}

// takeSyncWaiters hands the Run loop the requests the sync it is about to
// start answers.
func (s *Server) takeSyncWaiters() chan struct{} {
	q := &s.syncReqs
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.next
	q.next = nil
	return next
}

// awaitSync waits for an early sync; false when none ran in time or no Run
// loop is there to run it.
func (s *Server) awaitSync(ctx context.Context) bool {
	if !s.running.Load() {
		return false
	}
	done := s.requestSync()
	timer := time.NewTimer(catchUpWait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
	case <-timer.C:
	}
	return false
}
