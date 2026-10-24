package kv

import (
	"context"
	"fmt"
)

// ErrPoolBusy fails a call refused unsent because the store's connection wait queue was at its bound. It is an ErrUnavailable whose command certainly did not run.
var ErrPoolBusy = fmt.Errorf("%w: %w: connection wait queue full", ErrUnavailable, ErrNotRun)

// PoolShedder is implemented by a store whose calls can queue for a pooled connection. Every call counts toward the queue; only the OrShed calls are refused past its bound, so admission answers at once instead of waiting out the pool timeout to be refused anyway.
type PoolShedder interface {
	// ExecBatchOrShed is ExecBatch, except that while the wait queue is at its bound every op fails with ErrPoolBusy and nothing is sent.
	ExecBatchOrShed(ctx context.Context, ops []BatchOp)
	// RunScriptOrShed is RunScript, except that while the wait queue is at its bound it fails with ErrPoolBusy and sends nothing.
	RunScriptOrShed(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error)
	// PoolBusy reports whether an OrShed call made now would be refused. A lock-free snapshot that reserves nothing, for refusing work before it reaches the store.
	PoolBusy() bool
}
