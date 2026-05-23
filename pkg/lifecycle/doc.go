// Package lifecycle is the in-process hook registry every request-runner
// (pipeline, proxy, future ws/batch) fires events into. Observers
// (usage emit, OTel span export, audit log, anything cross-cutting)
// register hooks at boot; runners iterate the registry without knowing
// who's listening.
//
// Dependency direction (load-bearing):
//
//	app/pipeline ─┐
//	app/proxy    ─┼──→ pkg/lifecycle ←──┬─ app/usagelog
//	app/ws       ─┤   (registry +       ├─ app/otel    (future)
//	app/batch    ─┘    Event types)     └─ app/audit   (future)
//
// Request-runners never import observer packages. Observers never
// import request-runners. Both depend on this package's typed
// vocabulary. New observers slot in by registering at the composition
// root; runners stay untouched.
//
// Scope rules:
//
//   - Synchronous fan-out from a runner's detached post-flight
//     goroutine. Hooks must be non-blocking (enqueue + return). The
//     registry does not own queuing; that's a hook's internal concern.
//   - Typed event + typed hook signatures. No interface{} events, no
//     stringly-typed topics. If a new event kind appears, it gets a
//     new typed registration method (RegisterX / FireX) — the registry
//     grows by addition, never by string-keyed indirection.
//   - In-process only. Cross-process fan-out (NATS, Kafka, etc.) is a
//     concrete hook implementation, not a property of this package.
package lifecycle
