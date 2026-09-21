package sse

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

var testFrame = []byte(": keepalive\n\n")

func countFrames(b []byte) int { return bytes.Count(b, testFrame) }

// An upstream gap longer than the interval must produce a keepalive — one per elapsed interval, so a gap of 1.5 intervals yields exactly one.
func TestKeepAlive_IdleGapEmitsFrame(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 200*time.Millisecond)
	k.Start(context.Background())
	time.Sleep(300 * time.Millisecond)
	k.Stop()

	if got := countFrames(w.Body.Bytes()); got != 1 {
		t.Fatalf("keepalive frames during a 1.5-interval gap = %d, want 1", got)
	}
}

// A stream that keeps writing must never get a keepalive spliced into it.
func TestKeepAlive_ActiveStreamEmitsNothing(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 50*time.Millisecond)
	k.Start(context.Background())
	for range 20 {
		if _, err := k.Write([]byte("data: x\n\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	k.Stop()

	if got := countFrames(w.Body.Bytes()); got != 0 {
		t.Fatalf("keepalive frames during an active stream = %d, want 0", got)
	}
}

// A copy loop that stopped mid-frame (a 32 KiB read that didn't land on a frame boundary) must not have a keepalive appended into the partial frame.
func TestKeepAlive_PartialFrameBlocksKeepAlive(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 50*time.Millisecond)
	k.Start(context.Background())
	if _, err := k.Write([]byte("data: half")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	k.Stop()

	if got := countFrames(w.Body.Bytes()); got != 0 {
		t.Fatalf("keepalive frames after a partial frame = %d, want 0", got)
	}
}

// Stop joins the goroutine, so nothing can be written after the real stream ended.
func TestKeepAlive_StopIsFinalAndIdempotent(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 20*time.Millisecond)
	k.Start(context.Background())
	k.Stop()
	k.Stop()
	before := w.Body.Len()
	time.Sleep(100 * time.Millisecond)
	if w.Body.Len() != before {
		t.Fatalf("bytes written after Stop: %q", w.Body.String()[before:])
	}
}

func TestKeepAlive_DisabledPassesThrough(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 0)
	k.Start(context.Background())
	if _, err := k.Write([]byte("data: x\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	k.Stop()

	if got := w.Body.String(); got != "data: x\n\n" {
		t.Fatalf("body = %q, want the write passed through untouched", got)
	}
}

// A cancelled request context ends the keepalive without a Stop call.
func TestKeepAlive_ContextCancelStopsLoop(t *testing.T) {
	w := httptest.NewRecorder()
	k := NewKeepAlive(w, testFrame, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	k.Start(ctx)
	cancel()
	time.Sleep(120 * time.Millisecond)
	k.Stop()

	if got := countFrames(w.Body.Bytes()); got != 0 {
		t.Fatalf("keepalive frames after context cancel = %d, want 0", got)
	}
}
