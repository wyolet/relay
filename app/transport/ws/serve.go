package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Defaults for Options left zero by the caller.
const (
	defaultMaxInFlight = 64
	defaultReadLimit   = 1 << 20 // 1 MiB per inbound frame
	outboundBuffer     = 256
)

// Options tunes one served connection.
type Options struct {
	// MaxInFlight caps concurrent in-flight requests on the connection.
	// Frames arriving while saturated get an immediate 429 end frame.
	MaxInFlight int
	// ReadLimit is the max inbound message size in bytes.
	ReadLimit int64
	// PerRequest, if set, derives the per-frame request context from the
	// connection context — e.g. to stamp a fresh request id per frame so
	// multiplexed requests trace independently. Identity if nil.
	PerRequest func(parent context.Context) context.Context
	Logger     *slog.Logger
}

func (o Options) maxInFlight() int {
	if o.MaxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return o.MaxInFlight
}

func (o Options) readLimit() int64 {
	if o.ReadLimit <= 0 {
		return defaultReadLimit
	}
	return o.ReadLimit
}

// Serve runs the read/write pumps for one connection until the client
// disconnects or ctx is cancelled. base is the upgrade request; its
// context already carries authentication + classification from the
// middleware chain, and each frame is dispatched as a clone of it. The
// handler is invoked once per frame with a synthetic ResponseWriter.
//
// coder/websocket allows a single concurrent reader and a single
// concurrent writer: this loop is the only reader, and a dedicated
// goroutine is the only writer. Request goroutines never touch conn.
func Serve(ctx context.Context, conn *websocket.Conn, base *http.Request, handler http.HandlerFunc, opts Options) error {
	conn.SetReadLimit(opts.readLimit())

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan serverFrame, outboundBuffer)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		writePump(connCtx, conn, out, cancel, opts.Logger)
	}()

	sem := make(chan struct{}, opts.maxInFlight())
	var reqWG sync.WaitGroup

	readErr := readPump(connCtx, conn, base, handler, out, sem, &reqWG, opts)

	// Stop accepting work, let in-flight requests drain, then close the
	// outbound channel so the write pump exits.
	cancel()
	reqWG.Wait()
	close(out)
	writerWG.Wait()

	conn.Close(websocket.StatusNormalClosure, "")
	return readErr
}

// readPump is the sole reader. It decodes one clientFrame per message and
// dispatches each in its own goroutine, bounded by sem. Returns when the
// connection closes or ctx is cancelled.
func readPump(ctx context.Context, conn *websocket.Conn, base *http.Request, handler http.HandlerFunc, out chan<- serverFrame, sem chan struct{}, reqWG *sync.WaitGroup, opts Options) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var cf clientFrame
		if err := json.Unmarshal(data, &cf); err != nil || cf.ID == "" {
			// Can't correlate a malformed frame to a request id; drop it.
			if opts.Logger != nil {
				opts.Logger.Warn("ws: dropping malformed frame", "err", err)
			}
			continue
		}

		select {
		case sem <- struct{}{}:
		default:
			// Saturated: reject without blocking the reader.
			sendSaturated(ctx, out, cf.ID)
			continue
		}

		reqWG.Add(1)
		go func(cf clientFrame) {
			defer reqWG.Done()
			defer func() { <-sem }()
			dispatchFrame(ctx, base, handler, out, cf, opts)
		}(cf)
	}
}

// dispatchFrame builds the synthetic request + response writer for one
// frame and invokes the handler, emitting the terminal end frame after.
func dispatchFrame(ctx context.Context, base *http.Request, handler http.HandlerFunc, out chan<- serverFrame, cf clientFrame, opts Options) {
	reqCtx := ctx
	if opts.PerRequest != nil {
		reqCtx = opts.PerRequest(ctx)
	}
	r := base.Clone(reqCtx)
	r.Method = http.MethodPost
	body := io.NopCloser(bytes.NewReader(cf.Payload))
	r.Body = body
	r.ContentLength = int64(len(cf.Payload))

	w := newRespWriter(reqCtx, cf.ID, out)
	defer w.finish()

	defer func() {
		if rec := recover(); rec != nil && opts.Logger != nil {
			opts.Logger.Error("ws: handler panic", "id", cf.ID, "panic", rec)
		}
	}()
	handler(w, r)
}

// writePump is the sole writer. It serializes outbound frames onto the
// connection; any write error cancels the connection so the reader exits.
func writePump(ctx context.Context, conn *websocket.Conn, out <-chan serverFrame, cancel context.CancelFunc, logger *slog.Logger) {
	for f := range out {
		b, err := json.Marshal(f)
		if err != nil {
			continue
		}
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			if logger != nil {
				logger.Debug("ws: write failed, closing", "err", err)
			}
			cancel()
			// Keep draining out until the serve loop closes it so request
			// goroutines blocked on send unblock via cancelled ctx.
			for range out {
			}
			return
		}
	}
}

func sendSaturated(ctx context.Context, out chan<- serverFrame, id string) {
	frames := []serverFrame{
		{ID: id, Event: eventStart, Status: http.StatusTooManyRequests, Headers: map[string]string{"content-type": "application/json"}},
		{ID: id, Event: eventChunk, Data: `{"error":{"type":"rate_limit_error","code":"connection_saturated","message":"too many in-flight requests on this connection"}}`},
		{ID: id, Event: eventEnd, Status: http.StatusTooManyRequests},
	}
	for _, f := range frames {
		select {
		case out <- f:
		case <-ctx.Done():
			return
		}
	}
}
