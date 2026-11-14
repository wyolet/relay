package inference

import (
	"sync/atomic"
	"testing"
	"time"
)

// idleBody is a proxy body none of which has been read from the client.
func idleBody() *scannedBody { return newScannedBody(&modelScanner{}, nil, -1, nil) }

func awaitWatchExit(t *testing.T, fw *forwardWatch) {
	t.Helper()
	select {
	case <-fw.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never ended")
	}
}

// A body that stops moving is refused as a stall, the forward is cancelled, and the upstream response is aborted whether it was in hand before the stall or arrives after it.
func TestForwardWatch_Stall(t *testing.T) {
	for _, trackFirst := range []bool{true, false} {
		body := idleBody()
		var cancelled, aborted atomic.Int32
		abort := func() { aborted.Add(1) }
		// Held across the first ticks, so the abort is tracked before the stall is declared.
		body.mu.Lock()
		fw := watchForward(body, time.Millisecond, func() { cancelled.Add(1) })
		if trackFirst && !fw.track(abort) {
			t.Fatal("track before the stall reported the forward gone")
		}
		body.mu.Unlock()
		awaitWatchExit(t, fw)
		if !trackFirst && fw.track(abort) {
			t.Fatal("track after the stall reported the forward live")
		}
		if rej := body.rejection(); rej != errForwardStalled || cancelled.Load() != 1 || aborted.Load() != 1 {
			t.Fatalf("track first %v: refusal %v, %d cancels, %d aborts; want the stall, one cancel, one abort", trackFirst, rej, cancelled.Load(), aborted.Load())
		}
		fw.stop()
	}
}

// The watch ends without refusing anything once the body is read to its end or is already refused, and never starts without an idle period.
func TestForwardWatch_EndsQuietly(t *testing.T) {
	for name, tc := range map[string]struct {
		idle    time.Duration
		prepare func(*scannedBody)
		want    *bodyError
	}{
		"body read to its end": {time.Millisecond, func(b *scannedBody) { b.drained.Store(true) }, nil},
		"body already refused": {time.Millisecond, func(b *scannedBody) { _ = b.refuse(errTrailingBytes) }, errTrailingBytes},
		"no idle period":       {0, func(*scannedBody) {}, nil},
	} {
		body := idleBody()
		tc.prepare(body)
		fw := watchForward(body, tc.idle, func() { t.Errorf("%s: the forward was cancelled", name) })
		awaitWatchExit(t, fw)
		if !fw.track(func() { t.Errorf("%s: the response was aborted", name) }) || body.rejection() != tc.want {
			t.Errorf("%s: refusal %v, want %v on a forward still live", name, body.rejection(), tc.want)
		}
		fw.stop()
	}
}

// A tick that arrives together with the stop must not refuse a finished forward. Which of the two the watch sees first is the runtime's choice, so the pair is raced many times.
func TestForwardWatch_StopWinsOverAPendingTick(t *testing.T) {
	for range 40 {
		body := idleBody()
		// The watch takes its first tick and waits on the body's lock, while the next tick comes due.
		body.mu.Lock()
		fw := watchForward(body, time.Millisecond, func() { t.Error("a stopped forward was cancelled") })
		time.Sleep(4 * time.Millisecond)
		body.total.Add(1)
		go fw.stop()
		<-fw.done
		body.mu.Unlock()
		fw.stop()
		if rej := body.rejection(); rej != nil {
			t.Fatalf("a stopped forward was refused: %v", rej)
		}
	}
}
