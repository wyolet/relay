package inference

import (
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

// bodyForward follows the upstream transport's use of a streamed request body through httptrace, so the handler neither answers nor returns while the transport may still read the client's body. An answer written meanwhile lets the HTTP/1 server handle the unread body under the transport's reads, and a read after the handler returns runs into the connection's next request.
type bodyForward struct {
	started atomic.Bool
	sent    chan struct{}
	once    sync.Once
}

func newBodyForward() *bodyForward { return &bodyForward{sent: make(chan struct{})} }

func (f *bodyForward) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() { f.started.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { f.once.Do(func() { close(f.sent) }) },
	}
}

// wait reports whether the transport is done with the body, once the call to it has returned. An upstream that answers before the body is sent leaves the transport sending it; wait gives up when the body makes no progress for idle (0 waits as long as the client keeps it open).
func (f *bodyForward) wait(body *scannedBody, idle time.Duration) bool {
	if !f.started.Load() {
		return true
	}
	if idle <= 0 {
		<-f.sent
		return true
	}
	tick := time.NewTicker(idle)
	defer tick.Stop()
	last := body.total.Load()
	for {
		select {
		case <-f.sent:
			return true
		case <-tick.C:
			n := body.total.Load()
			if n == last {
				return false
			}
			last = n
		}
	}
}

// stop ends a forward wait gave up on and waits for the transport to let go of the body: no further reads, a pending read of the client cut short, and the upstream request cancelled so a blocked write fails.
func (f *bodyForward) stop(w http.ResponseWriter, body *scannedBody, cancel func()) {
	body.stopped.Store(true)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	cancel()
	<-f.sent
}

// markConnection closes the client's connection after the response unless its request body was read to the end: a remainder left on the wire would be parsed as the next request.
func markConnection(w http.ResponseWriter, body *scannedBody) {
	if body != nil && body.drained.Load() {
		w.Header().Del("Connection")
		return
	}
	w.Header().Set("Connection", "close")
}
