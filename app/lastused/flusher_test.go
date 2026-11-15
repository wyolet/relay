package lastused

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeWriter records each call's entries and keeps the later time per id, as the tables do.
type fakeWriter struct {
	mu                 sync.Mutex
	keyCalls, hkCalls  [][]Entry
	keys, hostKeys     map[string]time.Time
	failKeys, failHost error
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{keys: map[string]time.Time{}, hostKeys: map[string]time.Time{}}
}

func (w *fakeWriter) touch(calls *[][]Entry, stored map[string]time.Time, fail error, used []Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	*calls = append(*calls, append([]Entry(nil), used...))
	if fail != nil {
		return fail
	}
	for _, e := range used {
		if e.At.After(stored[e.ID]) {
			stored[e.ID] = e.At
		}
	}
	return nil
}

func (w *fakeWriter) TouchKeys(_ context.Context, used []Entry) error {
	return w.touch(&w.keyCalls, w.keys, w.failKeys, used)
}

func (w *fakeWriter) TouchHostKeys(_ context.Context, used []Entry) error {
	return w.touch(&w.hkCalls, w.hostKeys, w.failHost, used)
}

func minuteOf(t time.Time) time.Time { return t.Truncate(time.Minute) }

func TestFlushWritesOneSortedBatchPerKind(t *testing.T) {
	now := t0
	rec := newTestRecorder(0, &now)
	for _, id := range []string{"k-c", "k-a", "k-b"} {
		rec.Collect(keyRequest(id, "hk-"+id[2:]))
	}
	w := newFakeWriter()
	f := NewFlusher(rec, w, time.Minute, nil)
	if err := f.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	at := minuteOf(t0)
	wantKeys := [][]Entry{{{"k-a", at}, {"k-b", at}, {"k-c", at}}}
	wantHost := [][]Entry{{{"hk-a", at}, {"hk-b", at}, {"hk-c", at}}}
	if diff := cmp.Diff(wantKeys, w.keyCalls); diff != "" {
		t.Errorf("key batches (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantHost, w.hkCalls); diff != "" {
		t.Errorf("host key batches (-want +got):\n%s", diff)
	}

	// Nothing noted since: no statement at all.
	if err := f.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.keyCalls) != 1 || len(w.hkCalls) != 1 {
		t.Errorf("an empty flush wrote: %d key, %d host key batches", len(w.keyCalls), len(w.hkCalls))
	}
}

func TestFlushRetriesWhatAWriteFailedFor(t *testing.T) {
	now := t0
	rec := newTestRecorder(0, &now)
	rec.Collect(keyRequest("k-1", "hk-1"))
	w := newFakeWriter()
	w.failKeys = errors.New("pg down")
	f := NewFlusher(rec, w, time.Minute, nil)
	if err := f.Flush(context.Background()); err == nil {
		t.Fatal("Flush = nil, want the write error")
	}
	// Host keys were written; only the keys come back.
	now = t0.Add(2 * time.Minute)
	rec.Collect(keyRequest("k-2", ""))
	w.failKeys = nil
	if err := f.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"k-1": minuteOf(t0), "k-2": minuteOf(now)}
	if diff := cmp.Diff(want, w.keys); diff != "" {
		t.Errorf("stored keys (-want +got):\n%s", diff)
	}
	if len(w.hkCalls) != 1 {
		t.Errorf("host key batches = %d, want 1 (the written ones are not retried)", len(w.hkCalls))
	}
}

// Two pods flushing the same key: whatever order their writes land in, the later minute stays.
func TestTwoFlushersKeepTheLaterTime(t *testing.T) {
	early, late := t0, t0.Add(5*time.Minute)
	recA, recB := newTestRecorder(0, &early), newTestRecorder(0, &late)
	recA.Collect(keyRequest("k-1", "hk-1"))
	recB.Collect(keyRequest("k-1", "hk-1"))
	w := newFakeWriter()
	fA, fB := NewFlusher(recA, w, time.Minute, nil), NewFlusher(recB, w, time.Minute, nil)
	if err := fB.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fA.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.keys["k-1"]; !got.Equal(minuteOf(late)) {
		t.Errorf("stored = %v, want %v", got, minuteOf(late))
	}
}

func TestRunFlushesOnceMoreWhenStopped(t *testing.T) {
	now := t0
	rec := newTestRecorder(0, &now)
	w := newFakeWriter()
	f := NewFlusher(rec, w, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	rec.Collect(keyRequest("k-1", ""))
	cancel()
	<-done
	if _, ok := w.keys["k-1"]; !ok {
		t.Errorf("stored keys = %v, want k-1 from the final flush", w.keys)
	}
}
