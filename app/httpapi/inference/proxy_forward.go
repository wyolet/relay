package inference

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// forwardWatch gives up on a forward whose request body stops moving before its end: an upstream that neither answers nor reads holds the transport's write (and, over HTTP/2, a writer parked on flow control) for good, and a client that leaves mid-upload goes unnoticed while nothing reads its connection. Once the body is read to its end the server's own read of the connection notices a departing client, so the watch ends there.
type forwardWatch struct {
	body *scannedBody
	done chan struct{}

	mu     sync.Mutex
	result io.Closer // the upstream response body, once the call returned
	gone   bool
}

// watchForward checks every idle that the body moved; on a stall it refuses the body (so the call reports the stall, and post-flight records it), cancels the forward and closes a response already in hand. idle <= 0 watches nothing.
func watchForward(body *scannedBody, idle time.Duration, cancel func()) *forwardWatch {
	fw := &forwardWatch{body: body, done: make(chan struct{})}
	if idle <= 0 {
		return fw
	}
	go func() {
		tick := time.NewTicker(idle)
		defer tick.Stop()
		last := body.total.Load()
		for {
			select {
			case <-fw.done:
				return
			case <-tick.C:
			}
			if body.drained.Load() || body.Refusal() != nil {
				return
			}
			if n := body.total.Load(); n != last {
				last = n
				continue
			}
			_ = body.refuse(errForwardStalled)
			cancel()
			fw.mu.Lock()
			fw.gone = true
			result := fw.result
			fw.mu.Unlock()
			if result != nil {
				_ = result.Close()
			}
			return
		}
	}()
	return fw
}

// track hands the watch the upstream response body to close on a stall; it reports false when the stall came first, and then closes it itself.
func (fw *forwardWatch) track(result io.Closer) bool {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.gone {
		_ = result.Close()
		return false
	}
	fw.result = result
	return true
}

func (fw *forwardWatch) stop() { close(fw.done) }

// markConnection closes the client's connection after the response unless its request body was read to the end: a remainder left on the wire would be parsed as the next request. It also keeps the HTTP/1 server from draining the body under the transport's own reads when the answer is written first; after the handler returns the server cuts any such read short.
func markConnection(w http.ResponseWriter, body *scannedBody) {
	if body != nil && body.drained.Load() {
		w.Header().Del("Connection")
		return
	}
	w.Header().Set("Connection", "close")
}
