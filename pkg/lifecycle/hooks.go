package lifecycle

import "context"

// PreFlightMiddleware runs synchronously in the request's hot path
// before the upstream call. Sequential in registration order. Returning
// a non-nil error aborts the request and propagates the error to the
// runner's caller.
//
// May mutate lc and ev fields. Typical use: budget check, cache
// lookup with short-circuit, request enrichment, additional authz.
type PreFlightMiddleware func(ctx context.Context, lc *Context, ev *PreFlightEvent) error

// Hook fills its own result struct from the request's read-only state
// (the Context's identity/timing + the PostFlightEvent). It is a pure
// producer: it MUST NOT mutate the Context. The Registry calls Fill,
// then — as the sole writer — attaches the returned value to the Context
// under Name(). Because the hook never holds the write side of the
// collected set, it cannot break Context consistency or race a sibling.
//
// Fill returns (nil, nil) when the hook has nothing to contribute for
// this request (e.g. no usage block); the Registry attaches nothing.
//
// Fill must be cheap and non-blocking — heavy work (disk, network)
// belongs in a Collector behind a bounded channel, not here.
type Hook interface {
	Name() string
	Fill(lc *Context, ev *PostFlightEvent) (any, error)
}

// HookFunc adapts a plain function to the Hook interface — for tests and
// simple observers that capture state without producing a stored result
// (return nil, nil).
type HookFunc struct {
	HookName string
	Fn       func(lc *Context, ev *PostFlightEvent) (any, error)
}

func (h HookFunc) Name() string { return h.HookName }

func (h HookFunc) Fill(lc *Context, ev *PostFlightEvent) (any, error) {
	return h.Fn(lc, ev)
}

// StreamObserver is a per-request, stateful observer of a streamed
// response. Unlike a Hook (one-shot, post-flight, too late for a streamed
// body), it watches frames as they flow: Observe is called once per
// upstream frame, then Result is called at end-of-stream. The runner
// attaches Result's value to the Context under the factory's Name() and
// marks the request filled — so the post-flight sink reuses the same
// collection (collect once), exactly like the buffered path.
//
// Built fresh per request (see StreamObserverFactory) so per-stream state
// doesn't leak across concurrent requests — mirrors the v1 stream
// translator closures.
type StreamObserver interface {
	Observe(frame []byte)
	Result() (any, error)
}

// StreamObserverFactory produces a fresh StreamObserver per streamed
// request. Registered once at boot.
type StreamObserverFactory interface {
	Name() string
	NewObserver(lc *Context) StreamObserver
}

// Collector (the janitor) runs once at the end of the lifecycle, after
// every Hook has filled and the Registry has attached their results to
// the Context. It reads the collected results off the Context (via
// Context.Collected) and routes them to sinks — the "store" half of the
// produce → attach → store flow.
//
// Collectors run in parallel and treat the Context as read-only. Storing
// must be non-blocking (push onto a bounded channel); a Collector must
// never block the post-flight goroutine.
type Collector interface {
	Collect(lc *Context)
}
