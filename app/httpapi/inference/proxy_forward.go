package inference

import (
	"net/http"
	"sync"
	"time"
)

// forwardWatch gives up on a forward whose request body stops moving before its end: an upstream that neither answers nor reads holds the transport's write (and, over HTTP/2, a writer parked on flow control) for good, and a client that leaves mid-upload goes unnoticed while nothing reads its connection. Once the body is read to its end the server's own read of the connection notices a departing client, so the watch ends there. A pending read of the client is left to its idle deadline, which answers 408.
type forwardWatch struct {
	body   *scannedBody
	idle   time.Duration
	done   chan struct{}
	exited chan struct{}
	once   sync.Once

	mu    sync.Mutex
	abort func() // closes the upstream response under the handler's read, once the call returned
	gone  bool
}

// watchForward checks every idle that the body moved; on a stall it refuses the body (so the call reports the stall, and post-flight records it), cancels the forward and aborts a response already in hand. idle <= 0 watches nothing.
func watchForward(body *scannedBody, idle time.Duration, cancel func()) *forwardWatch {
	fw := &forwardWatch{body: body, idle: idle, done: make(chan struct{}), exited: make(chan struct{})}
	if idle <= 0 {
		close(fw.exited)
		return fw
	}
	go func() {
		defer close(fw.exited)
		tick := time.NewTicker(idle)
		defer tick.Stop()
		last := body.total.Load()
		for {
			select {
			case <-fw.done:
				return
			case <-tick.C:
			}
			select {
			case <-fw.done: // a tick delivered with the stop must not refuse a finished forward
				return
			default:
			}
			if body.drained.Load() || body.Refusal() != nil {
				return
			}
			if n := body.total.Load(); n != last || fw.clientReadPending() {
				last = n
				continue
			}
			_ = body.refuse(errForwardStalled)
			cancel()
			fw.mu.Lock()
			fw.gone = true
			abort := fw.abort
			fw.mu.Unlock()
			if abort != nil {
				abort()
			}
			return
		}
	}()
	return fw
}

// clientReadPending reports a read of the client still within reach of its idle deadline; one pending for twice that means the connection takes no deadline, and the stall is the watch's to end.
func (fw *forwardWatch) clientReadPending() bool {
	since := fw.body.readingSince.Load()
	return since != 0 && time.Since(time.Unix(0, since)) < 2*fw.idle
}

// track hands the watch the abort for the upstream response; it reports false when the stall came first, and then aborts itself.
func (fw *forwardWatch) track(abort func()) bool {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.gone {
		if abort != nil {
			abort()
		}
		return false
	}
	fw.abort = abort
	return true
}

// stop ends the watch and waits for it, so no refusal is recorded after it returns.
func (fw *forwardWatch) stop() {
	fw.once.Do(func() { close(fw.done) })
	<-fw.exited
}

// markConnection closes the client's connection after the response unless its request body was read to the end: a remainder left on the wire would be parsed as the next request. It also keeps the HTTP/1 server from draining the body under the transport's own reads when the answer is written first; after the handler returns the server cuts any such read short, and later reads fail without touching the connection.
func markConnection(w http.ResponseWriter, body *scannedBody) {
	if body != nil && body.drained.Load() {
		w.Header().Del("Connection")
		return
	}
	w.Header().Set("Connection", "close")
}
