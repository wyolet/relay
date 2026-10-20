package sse

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// closableRecorder counts writes that land after the test marked the stream closed, standing in for a handler that has already returned.
type closableRecorder struct {
	*httptest.ResponseRecorder
	closed atomic.Bool
	late   atomic.Int32
}

func (c *closableRecorder) Write(p []byte) (int, error) {
	if c.closed.Load() {
		c.late.Add(1)
	}
	return c.ResponseRecorder.Write(p)
}

// An armed, idle keepalive must not hold a goroutine: open streams would otherwise each add a stack to every GC mark phase.
func TestKeepAlive_IdleStreamsHoldNoGoroutine(t *testing.T) {
	const n = 200
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := runtime.NumGoroutine()
	kas := make([]*KeepAlive, n)
	for i := range kas {
		kas[i] = NewKeepAlive(httptest.NewRecorder(), testFrame, time.Hour)
		kas[i].Start(ctx)
	}
	delta := runtime.NumGoroutine() - before
	for _, k := range kas {
		k.Stop()
	}

	t.Logf("goroutines for %d idle keepalives: %d (%.2f per stream)", n, delta, float64(delta)/n)
	if delta > 5 {
		t.Fatalf("goroutine delta for %d idle keepalives = %d, want ~0", n, delta)
	}
}

// Stop racing a firing callback: whatever the interleaving, nothing may be written once Stop has returned.
func TestKeepAlive_NoWriteAfterStop(t *testing.T) {
	recs := make([]*closableRecorder, 300)
	for i := range recs {
		rec := &closableRecorder{ResponseRecorder: httptest.NewRecorder()}
		recs[i] = rec
		k := NewKeepAlive(rec, testFrame, 20*time.Microsecond)
		k.Start(context.Background())
		time.Sleep(time.Duration(i%7) * 15 * time.Microsecond)
		if i%2 == 0 {
			_, _ = k.Write([]byte("data: x\n\n"))
		}
		k.Stop()
		rec.closed.Store(true)
	}
	time.Sleep(20 * time.Millisecond)

	frames := 0
	for _, rec := range recs {
		if n := rec.late.Load(); n != 0 {
			t.Fatalf("%d writes landed after Stop returned", n)
		}
		frames += countFrames(rec.Body.Bytes())
	}
	t.Logf("keepalive frames written before Stop across %d streams: %d", len(recs), frames)
}

// Cancelling the request context stops future frames even though the stream emitted some before.
func TestKeepAlive_ContextCancelStopsFutureFrames(t *testing.T) {
	rec := &closableRecorder{ResponseRecorder: httptest.NewRecorder()}
	k := NewKeepAlive(rec, testFrame, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	k.Start(ctx)
	time.Sleep(70 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)
	k.mu.Lock()
	got := countFrames(rec.Body.Bytes())
	k.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	k.mu.Lock()
	after := countFrames(rec.Body.Bytes())
	k.mu.Unlock()
	k.Stop()

	if got == 0 {
		t.Fatalf("no keepalive frames before cancel")
	}
	if after != got {
		t.Fatalf("keepalive frames after cancel: %d, want %d", after, got)
	}
}

// A partial frame blocks keepalives only until a write completes the frame; silence after that yields a frame spliced after the boundary.
func TestKeepAlive_BoundaryWriteReenablesFrame(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 30*time.Millisecond)
	k.Start(context.Background())
	if _, err := k.Write([]byte("data: half")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	k.mu.Lock()
	mid := countFrames(w.Body.Bytes())
	k.mu.Unlock()
	if mid != 0 {
		t.Fatalf("keepalive frames after a partial frame = %d, want 0", mid)
	}
	if _, err := k.Write([]byte("\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	k.Stop()

	want := append([]byte("data: half\n\n"), testFrame...)
	if !bytes.HasPrefix(w.Body.Bytes(), want) {
		t.Fatalf("body = %q, want a keepalive right after the completed frame", w.Body.String())
	}
}

type discardFlusher struct{ h http.Header }

func (d *discardFlusher) Header() http.Header         { return d.h }
func (d *discardFlusher) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardFlusher) WriteHeader(int)             {}
func (d *discardFlusher) Flush()                      {}

// One op is one stream: Start, 64 writes of a ~200 B SSE frame, Stop.
func BenchmarkKeepAliveWrite(b *testing.B) {
	frame := append(bytes.Repeat([]byte("x"), 192), "\n\n"...)
	frame = append([]byte("data: "), frame...)
	w := &discardFlusher{h: http.Header{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ReportAllocs()
	for b.Loop() {
		k := NewKeepAlive(w, testFrame, 15*time.Second)
		k.Start(ctx)
		for range 64 {
			_, _ = k.Write(frame)
		}
		k.Stop()
	}
}
