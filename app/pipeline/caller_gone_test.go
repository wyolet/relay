package pipeline

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// callerGoneAdapter cancels the caller mid-call and fails the way the transport can then report it: a broken pipe on the request write, not the context error.
type callerGoneAdapter struct {
	reservationTestAdapter
	leave func()
	calls *int
}

func (a callerGoneAdapter) Call(ctx context.Context, _ string, _ *string, _ string, _ []byte, _ http.Header, _ string, _ bool, _ bool) (*http.Response, error) {
	*a.calls++
	a.leave()
	<-ctx.Done()
	return nil, &url.Error{Op: "Post", URL: "http://upstream/v1/messages", Err: &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}}
}

// A burst of callers leaving mid-call, more than the breaker threshold per key, records no key failure and fails over nowhere: the next request is served on the first key.
func TestRunCallerGoneOpensNoKey(t *testing.T) {
	h := newTierFullHarness(t, 3, 0)
	calls := 0
	for i := range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		req := h.request(lifecycle.NewContext("req", "pipeline", time.Now()), h.hops)
		req.Adapter = callerGoneAdapter{leave: cancel, calls: &calls}
		_, err := h.p.Run(ctx, req)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request %d: err = %v, want context.Canceled", i, err)
		}
		if kind, _ := classifyFailure(err); kind != "client_canceled" {
			t.Fatalf("request %d: usage kind %q, want client_canceled", i, kind)
		}
	}
	if calls != 20 {
		t.Fatalf("upstream calls = %d, want 20 (no failover for a caller that left)", calls)
	}
	if n := len(h.store.Named("keypool.record_failure")); n != 0 {
		t.Fatalf("record_failure scripts = %d, want 0", n)
	}

	if _, err := h.run(t); err != nil {
		t.Fatalf("Run after the burst: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != "sk-key-1" {
		t.Fatalf("served on %v, want key-1", h.used)
	}
}
