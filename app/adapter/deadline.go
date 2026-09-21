package adapter

import (
	"context"
	"io"
	"time"
)

// callContext derives the deadline context for one upstream call. A buffered call gets a total cap; a streamed call gets none — its bound is the idle timer on the response body, because a stream may take arbitrarily long as long as bytes keep arriving.
//
// The returned cancel is handed to the response body and fires on Close, never when Call returns: the body outlives the call and is read by the handler afterwards.
func (a *specAdapter) callContext(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	if !stream && a.spec.syncTimeout > 0 {
		return context.WithTimeout(ctx, a.spec.syncTimeout)
	}
	if stream && a.spec.idleTimeout > 0 {
		return context.WithCancel(ctx)
	}
	return ctx, func() {}
}

// bindBody attaches cancel to the upstream body so the derived context is released exactly when the caller closes it, adding the idle deadline on a streamed body.
func (a *specAdapter) bindBody(rc io.ReadCloser, cancel context.CancelFunc, stream bool) io.ReadCloser {
	if stream && a.spec.idleTimeout > 0 {
		return &idleTimeoutBody{rc: rc, every: a.spec.idleTimeout, cancel: cancel, timer: time.AfterFunc(a.spec.idleTimeout, cancel)}
	}
	return &cancelOnCloseBody{ReadCloser: rc, cancel: cancel}
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// idleTimeoutBody cancels the request context when no byte arrives for `every`. One timer per stream, reset per read — no goroutine, no per-byte work.
//
// Once it fires the in-flight Read fails with the cancellation, which is exactly the signal the caller needs: the upstream went silent, this stream is over.
type idleTimeoutBody struct {
	rc     io.ReadCloser
	timer  *time.Timer
	every  time.Duration
	cancel context.CancelFunc
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err == nil {
		b.timer.Reset(b.every)
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}
