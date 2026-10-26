package wal

import (
	"bytes"
	"time"
)

// maxRetainedBytes bounds the records a queue keeps in memory for flushing,
// counted as encoded line bytes plus the record's own size. It matches the
// payload sink's default segment cap, so at most about one full payload
// segment (or a few usage segments) is held; past it, segments flush from disk.
const maxRetainedBytes = 64 << 20

// rotatedSegment is what the queue knows about a segment it rotated.
type rotatedSegment[T any] struct {
	lines int
	// records is the segment as its file decodes, held for the first flush
	// attempt; nil when the flush must read the file.
	records []T
	bytes   int
}

// retainLocked keeps rec for the active segment's flush while every line
// written to the segment so far is held. A record that only decoding could
// reproduce, or one past the memory bound, sends the whole segment to disk.
// Must be called with mu held, after line reached the writer.
func (q *Queue[T]) retainLocked(rec T, line []byte) {
	if !q.retaining {
		return
	}
	size := len(line) + q.recordSize
	if len(line) > scanBuf || mayHoldReplacedUTF8(line) || q.retainedBytes+size > q.retainLimit {
		q.stopRetainingLocked()
		return
	}
	dec, ok := q.decoded(rec)
	if !ok {
		q.stopRetainingLocked()
		return
	}
	if q.retained == nil {
		q.retained = make([]T, 0, min(q.retainCap, q.retainLimit/max(q.recordSize, 1)))
	}
	q.retained = append(q.retained, dec)
	q.activeRetainedBytes += size
	q.retainedBytes += size
}

// stopRetainingLocked drops the active segment's held records, so its flush
// reads the file. Must be called with mu held.
func (q *Queue[T]) stopRetainingLocked() {
	q.retainedBytes -= q.activeRetainedBytes
	q.retained, q.activeRetainedBytes, q.retaining = nil, 0, false
}

// startRetainingLocked decides whether a freshly opened active segment is
// held in memory: only when it starts empty, since earlier content (a torn
// line from a crash included) is known only to the file. Must be called with
// mu held.
func (q *Queue[T]) startRetainingLocked(size int64) {
	q.retained, q.activeRetainedBytes = nil, 0
	q.retaining = q.decoded != nil && size == 0 && !q.failing.Load()
}

// takeRecords hands over the held records of a rotated segment, leaving the
// file as the only copy for any later attempt.
func (q *Queue[T]) takeRecords(path string) ([]T, int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	seg, ok := q.segments[path]
	if !ok || seg.records == nil {
		return nil, 0, false
	}
	records, size := seg.records, seg.bytes
	seg.records, seg.bytes = nil, 0
	q.segments[path] = seg
	return records, size, true
}

// releaseRetained returns the budget of records a flush attempt was handed.
func (q *Queue[T]) releaseRetained(size int) {
	q.mu.Lock()
	q.retainedBytes -= size
	q.mu.Unlock()
}

// dropRetained releases every held record after a failed flush: retries and
// the segments that pile up during a sink outage read from disk.
func (q *Queue[T]) dropRetained() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopRetainingLocked()
	for path, seg := range q.segments {
		if seg.records != nil {
			q.retainedBytes -= seg.bytes
			seg.records, seg.bytes = nil, 0
			q.segments[path] = seg
		}
	}
}

// mayHoldReplacedUTF8 reports whether line contains U+FFFD, raw or escaped:
// the encoder writes it for invalid UTF-8, which then decodes differently
// from the record that was written.
func mayHoldReplacedUTF8(line []byte) bool {
	return bytes.Contains(line, []byte("�")) || bytes.Contains(line, []byte(`�`))
}

// DecodedTime returns t as a JSON round trip yields it (RFC 3339 with
// nanoseconds, no monotonic reading, the zone re-derived from the offset), for
// a decoded function passed to OpenRetaining. False when t has no JSON form.
func DecodedTime(t time.Time) (time.Time, bool) {
	var buf [64]byte
	b, err := t.AppendText(buf[:0])
	if err != nil {
		return time.Time{}, false
	}
	var out time.Time
	if err := out.UnmarshalText(b); err != nil {
		return time.Time{}, false
	}
	return out, true
}
