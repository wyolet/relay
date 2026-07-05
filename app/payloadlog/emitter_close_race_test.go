package payloadlog

import (
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// Audit 2026-07-04 (audit-app-services.md, P2): identical code shape to
// app/usagelog — Emit's stopped.Load() check-then-send races Close's
// close(e.queue); the losing goroutine panics with "send on closed channel"
// on a detached post-flight goroutine at shutdown. Correct behavior: Emit
// concurrent with Close must be a safe no-op/drop.
func TestEmitterEmitConcurrentWithCloseDoesNotPanic(t *testing.T) {
	t.Skip("audit 2026-07-04: emitter Emit/Close race panics at shutdown — known-broken, unskip with the fix")
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS > 1 to interleave Emit and Close")
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 4 {
		workers = 4
	}

	const trials = 20000
	for trial := 0; trial < trials; trial++ {
		e := NewEmitter(EmitterOptions{
			QueueSize: 64,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})

		var (
			panicked atomic.Value
			stop     atomic.Bool
			start    = make(chan struct{})
			ready    sync.WaitGroup
			wg       sync.WaitGroup
		)
		ready.Add(workers)
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						panicked.Store(fmt.Sprint(r))
					}
				}()
				<-start
				e.Emit(Record{})
				ready.Done()
				for !stop.Load() {
					e.Emit(Record{})
				}
			}()
		}
		close(start)
		ready.Wait() // all workers actively looping Emit
		e.Close()
		stop.Store(true)
		wg.Wait()

		if p := panicked.Load(); p != nil {
			t.Fatalf("trial %d: Emit racing Close must be a safe no-op/drop, but panicked: %v", trial, p)
		}
	}
}
