package lifecycle

import (
	"context"
	"log/slog"
	"sync"
)

// Registry stores registered hooks and dispatches lifecycle events
// to them. Construct one at the composition root, register hooks
// during boot, pass into every request-runner.
//
// Safe for concurrent Register* / Run* / Fire* calls. Registration
// is typically a boot-time operation though; the hot side is the
// dispatch methods.
type Registry struct {
	mu         sync.RWMutex
	preFlight  []PreFlightMiddleware
	postFlight []PostFlightHook
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{}
}

// --- registration ---

// RegisterPreFlight appends m to the pre-flight middleware chain.
// Middlewares run sequentially in registration order from
// RunPreFlight. Nil middlewares are silently skipped.
func (r *Registry) RegisterPreFlight(m PreFlightMiddleware) {
	if m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preFlight = append(r.preFlight, m)
}

// RegisterPostFlight appends h to the post-flight hook chain. Hooks
// fire in parallel from FirePostFlight; registration order is
// observable only via PostFlightCount, not via execution. Nil hooks
// are silently skipped.
func (r *Registry) RegisterPostFlight(h PostFlightHook) {
	if h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.postFlight = append(r.postFlight, h)
}

// --- dispatch ---

// RunPreFlight invokes every registered middleware sequentially with
// lc and ev. Returns the first non-nil error a middleware produces —
// no further middlewares run after an abort. Returns nil if every
// middleware returned nil or none are registered.
//
// Called synchronously from the runner before the upstream call. The
// returned error is the abort signal; the runner surfaces it to the
// HTTP caller via the normal error envelope.
func (r *Registry) RunPreFlight(ctx context.Context, lc *Context, ev *PreFlightEvent) error {
	r.mu.RLock()
	mws := r.preFlight
	r.mu.RUnlock()
	for _, m := range mws {
		if err := m(ctx, lc, ev); err != nil {
			return err
		}
	}
	return nil
}

// FirePostFlight invokes every registered observer hook in parallel
// with lc and ev, waits for all to complete, then returns. Panics
// inside a hook are recovered and logged; sibling hooks proceed.
//
// Called from the runner's detached post-flight goroutine. Blocks
// only that goroutine, never the caller. The wait gives accurate
// post-flight latency metrics and lets graceful shutdown drain
// in-flight observers.
func (r *Registry) FirePostFlight(ctx context.Context, lc *Context, ev *PostFlightEvent) {
	r.mu.RLock()
	hooks := r.postFlight
	r.mu.RUnlock()
	if len(hooks) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, h := range hooks {
		wg.Add(1)
		go func(hook PostFlightHook) {
			defer wg.Done()
			defer recoverPostFlight(lc)
			hook(ctx, lc, ev)
		}(h)
	}
	wg.Wait()
}

// recoverPostFlight isolates panics inside one observer from siblings
// and the post-flight goroutine. A panicking observer is a bug; we
// log and continue. Never propagated — observers must not be able to
// crash the runner.
func recoverPostFlight(lc *Context) {
	if v := recover(); v != nil {
		slog.Error("lifecycle: post-flight hook panic recovered",
			"request_id", lc.RequestID,
			"source", lc.Source,
			"panic", v,
		)
	}
}

// --- introspection ---

// PreFlightCount returns the number of registered middlewares.
func (r *Registry) PreFlightCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.preFlight)
}

// PostFlightCount returns the number of registered observer hooks.
func (r *Registry) PostFlightCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.postFlight)
}
