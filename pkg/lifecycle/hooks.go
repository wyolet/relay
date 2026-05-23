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

// PostFlightHook runs asynchronously in the runner's detached
// post-flight goroutine. Parallel across hooks (each in its own
// goroutine). Panics inside one hook are recovered and do not affect
// siblings or the runner.
//
// Pure observer: cannot abort, cannot mutate lc.Metadata (concurrent
// writes are a race), cannot mutate ev.ResponseBody (shared backing
// array). To transform, copy first.
//
// Hooks themselves must be non-blocking. Long-running work (disk
// write, network publish) belongs behind a bounded channel inside the
// hook implementation.
type PostFlightHook func(ctx context.Context, lc *Context, ev *PostFlightEvent)
