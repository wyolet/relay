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

// frameTerminator ends every SSE frame; a keepalive is only safe to splice in after one.
var frameTerminator = []byte("\n\n")

// KeepAlive wraps a streaming ResponseWriter and writes frame whenever nothing else has been written for the configured interval. It is an io.Writer + http.Flusher, so the copy loops can write through it unchanged.
//
// Safety of the splice rests on two gates, both required: the ticker only fires when no Write landed in the last interval (so it can never interleave with an in-progress write), and only when the last write ended on an SSE frame boundary (so a copy loop that hands over a half frame — streamCopy reads in 32 KiB blocks, not frames — is never cut in two).
type KeepAlive struct {
	w       http.ResponseWriter
	flusher http.Flusher
	frame   []byte
	every   time.Duration

	mu         sync.Mutex
	last       time.Time
	atBoundary bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

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
		stop:       make(chan struct{}),
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
	k.atBoundary = bytes.HasSuffix(p[:n], frameTerminator)
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

// Start launches the keepalive goroutine. It exits on ctx cancellation (client gone) or Stop. Call at most once, before the copy loop.
func (k *KeepAlive) Start(ctx context.Context) {
	if k.every <= 0 || len(k.frame) == 0 {
		return
	}
	k.done = make(chan struct{})
	go k.loop(ctx)
}

func (k *KeepAlive) loop(ctx context.Context) {
	defer close(k.done)
	t := time.NewTicker(k.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.stop:
			return
		case now := <-t.C:
			k.mu.Lock()
			if k.atBoundary && now.Sub(k.last) >= k.every {
				if _, err := k.w.Write(k.frame); err == nil {
					k.last = now
					k.flush()
				}
			}
			k.mu.Unlock()
		}
	}
}

// Stop ends the keepalive and waits for its goroutine, so no frame can land after the real stream finished. Idempotent; safe when Start was never called.
func (k *KeepAlive) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	if k.done != nil {
		<-k.done
	}
}
