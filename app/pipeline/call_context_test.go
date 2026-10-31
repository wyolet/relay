package pipeline

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func waitDone(t *testing.T, ctx context.Context, within time.Duration) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(within):
		t.Fatalf("call context still live after %v", within)
	}
}

// A caller leaving before a response is accepted aborts the upstream at once, with or without a call cap.
func TestUpstreamCallCallerLeavesBeforeAccept(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		parent, leave := context.WithCancel(context.Background())
		call := newUpstreamCall(parent, timeout)
		leave()
		waitDone(t, call.ctx, time.Second)
		if err := call.ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("timeout %v: err = %v, want canceled", timeout, err)
		}
		call.release()
	}
}

// Once a response is accepted the caller leaving (or the handler returning) leaves the body readable for drainTimeout, then cuts it.
func TestUpstreamCallCallerLeavesAfterAccept(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			t.Parallel()
			parent, leave := context.WithCancel(context.Background())
			call := newUpstreamCall(parent, timeout)
			defer call.release()
			call.accepted.Store(true)
			start := time.Now()
			leave()
			time.Sleep(100 * time.Millisecond)
			if err := call.ctx.Err(); err != nil {
				t.Fatalf("call context ended %v after the caller left: %v", time.Since(start), err)
			}
			waitDone(t, call.ctx, drainTimeout+time.Second)
			if waited := time.Since(start); waited < drainTimeout {
				t.Fatalf("cut after %v, want at least %v", waited, drainTimeout)
			}
		})
	}
}

// A call cap ends the call context with a deadline error and leaves the caller's context alone; release ends it before the cap.
func TestUpstreamCallTimeout(t *testing.T) {
	parent := context.Background()
	call := newUpstreamCall(parent, 50*time.Millisecond)
	if _, ok := call.ctx.Deadline(); !ok {
		t.Fatal("capped call context has no deadline")
	}
	waitDone(t, call.ctx, time.Second)
	if err := call.ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	call.release()

	call = newUpstreamCall(parent, time.Minute)
	call.release()
	if err := call.ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("after release err = %v, want canceled", err)
	}
	if _, ok := newUpstreamCall(parent, 0).ctx.Deadline(); ok {
		t.Fatal("uncapped call context has a deadline")
	}
}

// timeoutAdapter reports a call cap and waits on the call context, as a stalled upstream would.
type timeoutAdapter struct {
	reservationTestAdapter
	timeout  time.Duration
	deadline *time.Duration
}

func (a timeoutAdapter) CallTimeout(stream bool) time.Duration {
	if stream {
		return 0
	}
	return a.timeout
}

func (a timeoutAdapter) Call(ctx context.Context, _ string, _ *string, _ string, _ []byte, _ http.Header, _ string, stream bool, _ bool) (*http.Response, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		*a.deadline = 0
		return nil, errors.New("no deadline")
	}
	*a.deadline = time.Until(dl)
	<-ctx.Done()
	return nil, ctx.Err()
}

// The adapter's cap reaches the call context, an upstream past it fails the request with the deadline, and the key is not charged a failure.
func TestRunUpstreamTimeout(t *testing.T) {
	h := newTierFullHarness(t, 2, 0)
	var left time.Duration
	req := h.request(lifecycle.NewContext("req", "pipeline", time.Now()), h.hops)
	req.Adapter = timeoutAdapter{timeout: 100 * time.Millisecond, deadline: &left}
	_, err := h.p.Run(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if left <= 0 || left > 100*time.Millisecond {
		t.Fatalf("deadline seen by the adapter = %v away, want within the 100ms cap", left)
	}
	if n := len(h.store.Named("keypool.record_failure")); n != 0 {
		t.Fatalf("record_failure scripts = %d, want 0", n)
	}
}

// deadlineRecorder reports how far off post-flight's deadline was when it recorded reachability.
type deadlineRecorder chan time.Duration

func (d deadlineRecorder) Reachable(ctx context.Context, _ string) {
	dl, ok := ctx.Deadline()
	if !ok {
		d <- -1
		return
	}
	d <- time.Until(dl)
}
func (deadlineRecorder) Unreachable(context.Context, string, string) {}

// Post-flight runs detached from the caller but under its own 5 s bound.
func TestRunPostFlightBound(t *testing.T) {
	h := newTierFullHarness(t, 1, 0)
	seen := make(deadlineRecorder, 1)
	p := h.newPipeline()
	p.HostHealth = seen
	ctx, leave := context.WithCancel(context.Background())
	req := h.request(lifecycle.NewContext("req", "pipeline", time.Now()), h.hops)
	req.Adapter = reservationTestAdapter{}
	res, err := p.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = res.Body.Close()
	leave()
	select {
	case left := <-seen:
		if left <= 4*time.Second || left > 5*time.Second {
			t.Fatalf("post-flight deadline %v away, want just under 5s", left)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
}

// The tee is sized from a Content-Length only when one is known and small; a larger claim grows with the bytes that arrive.
func TestNewTeeBuffer(t *testing.T) {
	for _, tc := range []struct {
		contentLength int64
		wantCap       bool
	}{
		{-1, false},
		{0, false},
		{1500, true},
		{maxTeePresize, true},
		{maxTeePresize + 1, false},
		{1 << 40, false},
	} {
		got := cap(newTeeBuffer(tc.contentLength).AvailableBuffer())
		if tc.wantCap && int64(got) < tc.contentLength {
			t.Errorf("Content-Length %d: capacity %d, want at least that", tc.contentLength, got)
		}
		if !tc.wantCap && got != 0 {
			t.Errorf("Content-Length %d: capacity %d, want none up front", tc.contentLength, got)
		}
	}
}
