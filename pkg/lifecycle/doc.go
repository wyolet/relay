// Package lifecycle is the in-process hook registry every request-runner
// (pipeline, proxy, future ws/batch) fires events into. Two kinds of
// hook coexist; pick by what you actually need.
//
// # Two hook kinds
//
//	PreFlightMiddleware — synchronous, can abort
//	  Signature: func(ctx context.Context, lc *Context, ev *PreFlightEvent) error
//	  Runs in the request's hot path before the upstream call.
//	  Sequential (registration order). Returning a non-nil error aborts
//	  the request — the runner surfaces the error to the caller.
//	  May mutate lc.Metadata and ev.Request.
//	  Use for: budget check, cache lookup, response replay,
//	    per-model rate limit, additional authz that needs the resolved Plan.
//
//	PostFlightHook — asynchronous, observer-only
//	  Signature: func(ctx context.Context, lc *Context, ev *PostFlightEvent)
//	  Runs in the runner's detached post-flight goroutine, in parallel
//	  across hooks. Panics inside one hook are isolated and do not
//	  affect siblings or the runner.
//	  Use for: usage emit, OTel span export, audit log, webhook
//	    delivery — anything reactive.
//
// # Dependency direction
//
//	app/pipeline ─┐
//	app/proxy   ─┼──→ pkg/lifecycle ←──┬─ app/usagelog
//	app/ws      ─┤   (Context + Events  ├─ app/otel    (future)
//	app/batch   ─┘    + Registry)       └─ app/audit   (future)
//
// Request-runners never import observer packages. Observers never
// import request-runners. Both depend on this package's typed
// vocabulary. New observers slot in by registering at the composition
// root; runners stay untouched.
//
// # Context vs Events
//
// Context is the persistent lifecycle state — one instance per request,
// shared across every phase, mutable for middleware to enrich.
// Events are per-phase snapshots — what's true at this boundary. Both
// are passed to hooks by pointer; the hook signatures make the
// distinction explicit.
//
// # Sharing rules (load-bearing)
//
// PostFlight observers run in parallel. Two invariants:
//
//   - ResponseBody is read-only in post-flight. Multiple goroutines
//     read the same backing array; mutation is a race. To transform,
//     copy first via bytes.Clone.
//   - lc.Metadata is read-only in post-flight. Middlewares write it
//     (sequential — safe); observers read it. Concurrent map writes
//     panic.
//
// PreFlight middleware runs sequentially; mutation is fine.
//
// # Scope
//
//   - In-process only. Cross-process fan-out (NATS, Kafka, etc.) is a
//     concrete hook implementation, not a property of this package.
//   - Typed events + typed hook signatures. No interface{} events,
//     no stringly-typed topics. New event kinds add typed
//     Register*/Run*/Fire* methods to Registry.
package lifecycle
