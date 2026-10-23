package payloadlog

import (
	"sync"
	"unsafe"
)

// recordOverhead is what a queued Record costs besides its bodies and ids:
// its slot in the queue and in the drain's batch slice.
const recordOverhead = 2 * int(unsafe.Sizeof(Record{}))

// recordBytes is the memory r holds while it waits to be written.
func recordBytes(r Record) int {
	return len(r.RequestBody) + len(r.ResponseBody) + len(r.RequestID) +
		len(r.ProjectID) + len(r.PrincipalID) + len(r.RelayKeyHash) + recordOverhead
}

// recordQueue is a FIFO of Records bounded by the bytes they hold. Bytes
// stay counted from push until the drain releases them after the write.
type recordQueue struct {
	max int

	mu    sync.Mutex
	recs  []Record
	bytes int
}

// push appends r unless that would exceed the budget. An empty queue takes
// any one Record, so a body larger than the whole budget is still kept.
func (q *recordQueue) push(r Record) bool {
	n := recordBytes(r)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.bytes > 0 && q.bytes+n > q.max {
		return false
	}
	q.recs = append(q.recs, r)
	q.bytes += n
	return true
}

// swap hands back the queued Records and keeps spare, emptied, as the new
// queue.
func (q *recordQueue) swap(spare []Record) []Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	recs := q.recs
	q.recs = spare[:0]
	return recs
}

// release returns n bytes of written Records to the budget.
func (q *recordQueue) release(n int) {
	q.mu.Lock()
	q.bytes -= n
	q.mu.Unlock()
}

func (q *recordQueue) held() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

func (q *recordQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.recs)
}
