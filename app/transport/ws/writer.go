package ws

import (
	"context"
	"net/http"
)

// respWriter is the synthetic http.ResponseWriter handed to the injected
// handler for one frame. It captures the status + headers and turns each
// Write into a chunk frame, all tagged with the frame's id and pushed to
// the connection's single outbound channel. This is the seam that lets
// the unchanged HTTP dispatch path serve a WebSocket: the handler thinks
// it is writing an HTTP response.
//
// Sends respect ctx so a closed connection (ctx cancelled by the serve
// loop) drops frames instead of blocking a request goroutine forever.
type respWriter struct {
	id      string
	out     chan<- serverFrame
	ctx     context.Context
	header  http.Header
	status  int
	started bool
}

func newRespWriter(ctx context.Context, id string, out chan<- serverFrame) *respWriter {
	return &respWriter{id: id, out: out, ctx: ctx, header: make(http.Header)}
}

func (w *respWriter) Header() http.Header { return w.header }

// WriteHeader emits the start frame carrying the status and a minimal
// header set (Content-Type only — clients distinguish streaming SSE from
// a buffered JSON body by it). Subsequent calls are no-ops, matching
// net/http semantics.
func (w *respWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	w.status = status
	w.send(serverFrame{
		ID:      w.id,
		Event:   eventStart,
		Status:  status,
		Headers: map[string]string{"content-type": w.header.Get("Content-Type")},
	})
}

func (w *respWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if len(p) == 0 {
		return 0, nil
	}
	w.send(serverFrame{ID: w.id, Event: eventChunk, Data: string(p)})
	return len(p), nil
}

// Flush is a no-op: each Write already emits an independent chunk frame,
// so streaming flush points are honored by construction.
func (w *respWriter) Flush() {}

// finish ensures a start frame was emitted (so a handler that wrote
// nothing still produces start→end) and sends the terminal end frame.
func (w *respWriter) finish() {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	w.send(serverFrame{ID: w.id, Event: eventEnd, Status: w.status})
}

func (w *respWriter) send(f serverFrame) {
	select {
	case w.out <- f:
	case <-w.ctx.Done():
	}
}

var (
	_ http.ResponseWriter = (*respWriter)(nil)
	_ http.Flusher        = (*respWriter)(nil)
)
