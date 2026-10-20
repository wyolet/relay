// Package sse holds server-sent-events helpers for the response path. Today that is exactly one thing: KeepAlive, a response-writer wrapper that emits a caller-supplied no-op frame while the upstream is silent, so byte-watchdog clients — which abort a stream after N seconds without bytes — stay connected through a long prompt-processing or thinking pause.
//
// Deliberately out of scope: parsing, framing or rewriting SSE. KeepAlive never inspects the bytes it forwards beyond checking that the last write ended on a frame boundary.
package sse

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"
)

// endsFrame reports whether b ends on the blank line that ends an SSE frame, under any of the CRLF, LF and CR line endings; a keepalive is only safe to splice in after one.
func endsFrame(b []byte) bool {
	switch {
	case bytes.HasSuffix(b, []byte("\r\n")):
		b = b[:len(b)-2]
	case bytes.HasSuffix(b, []byte("\n")), bytes.HasSuffix(b, []byte("\r")):
		b = b[:len(b)-1]
	default:
		return false
	}
	return bytes.HasSuffix(b, []byte("\n")) || bytes.HasSuffix(b, []byte("\r"))
}

// KeepAlive wraps a streaming ResponseWriter and writes frame whenever nothing else has been written for the configured interval. It is an io.Writer + http.Flusher, so the copy loops can write through it unchanged.
//
// Safety of the splice rests on two gates, both required: a frame is only written when no Write landed in the last interval (so it can never interleave with an in-progress write), and only when the last write ended on an SSE frame boundary (so a copy loop that hands over a half frame — streamCopy reads in 32 KiB blocks, not frames — is never cut in two).
//
// It runs on a timer callback rather than a goroutine per stream, so an idle open stream holds no goroutine stack for the GC to scan.
type KeepAlive struct {
	w       http.ResponseWriter
	flusher http.Flusher
	frame   []byte
	every   time.Duration

	mu         sync.Mutex
	last       time.Time
	atBoundary bool
	stopped    bool
	timer      *time.Timer
	stopCtx    func() bool
}

// minRearm bounds how soon tick re-arms, so a tick landing just before the silence window closes cannot schedule a near-zero timer.
const minRearm = time.Millisecond

// NewKeepAlive returns a KeepAlive writing frame to w every interval of silence. every <= 0 disables it entirely: Write is a plain pass-through and Start/Stop are no-ops.
func NewKeepAlive(w http.ResponseWriter, frame []byte, every time.Duration) *KeepAlive {
	f, _ := w.(http.Flusher)
	return &KeepAlive{
		w:       w,
		flusher: f,
		frame:   frame,
		every:   every,
		// Response headers are already written by the time a copy loop starts, so the stream is at a frame boundary from the first tick on and needs no "wait for the first real write" flag.
		last:       time.Now(),
		atBoundary: true,
	}
}

func (k *KeepAlive) Write(p []byte) (int, error) {
	if k.every <= 0 {
		return k.w.Write(p)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	n, err := k.w.Write(p)
	k.last = time.Now()
	k.atBoundary = endsFrame(p[:n])
	k.flush()
	return n, err
}

// Flush satisfies http.Flusher so the wrapped writer keeps flushing per chunk. Writes already flush themselves; this exists for copy loops that flush explicitly.
func (k *KeepAlive) Flush() {
	if k.every <= 0 {
		if k.flusher != nil {
			k.flusher.Flush()
		}
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.flush()
}

func (k *KeepAlive) flush() {
	if k.flusher != nil {
		k.flusher.Flush()
	}
}

// Start arms the keepalive timer. It ends on ctx cancellation (client gone) or Stop. Call at most once, before the copy loop.
func (k *KeepAlive) Start(ctx context.Context) {
	if k.every <= 0 || len(k.frame) == 0 {
		return
	}
	// Held so tick (which may fire at once for a tiny interval) and Stop see timer and stopCtx assigned.
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	k.timer = time.AfterFunc(k.every, k.tick)
	k.stopCtx = context.AfterFunc(ctx, k.Stop)
}

// tick runs on the timer goroutine. Writes do not reset the timer (a timer-heap op per SSE event); instead tick re-arms for whatever is left of the current silence window.
func (k *KeepAlive) tick() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	now := time.Now()
	next := k.every
	if idle := now.Sub(k.last); idle < k.every {
		next = max(k.every-idle, minRearm)
	} else if k.atBoundary {
		if _, err := k.w.Write(k.frame); err == nil {
			k.last = now
			k.flush()
		}
	}
	k.timer.Reset(next)
}

// Stop ends the keepalive so no frame can land after the real stream finished: a tick either completed before Stop took the lock or sees stopped. Idempotent; safe when Start was never called.
func (k *KeepAlive) Stop() {
	k.mu.Lock()
	k.stopped = true
	timer, stopCtx := k.timer, k.stopCtx
	k.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if stopCtx != nil {
		stopCtx()
	}
}
