package usagelog

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// Emits run on detached post-flight goroutines nothing awaits, so an Emit
// concurrent with Close must be a safe no-op/drop — a panic there kills the
// process at shutdown. The window is concurrent-only, so this is a tight-loop
// harness with per-goroutine recover; -race widens the interleavings.
func TestEmitterEmitConcurrentWithCloseDoesNotPanic(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS > 1 to interleave Emit and Close")
	}
	// Fewer spinners than CPUs, so Close and the drain goroutine are not
	// starved waiting on preemption.
	workers := min(runtime.GOMAXPROCS(0)-1, 3)

	const trials = 2000
	for trial := 0; trial < trials; trial++ {
		e := NewEmitter(EmitterOptions{
			QueueSize: 64,
			Logger:    slog.New(slog.DiscardHandler),
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
				e.Emit(Event{})
				ready.Done()
				for !stop.Load() {
					e.Emit(Event{})
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
