package lifecycle

import (
	"context"
	"log/slog"
	"sync"
)

// Registry holds the registered pre-flight middleware, post-flight Hooks
// (producers), and Collectors (the janitors that store). Construct one at
// the composition root, register during boot, pass into every
// request-runner.
//
// Safe for concurrent Register* / Run* / Finalize calls. Registration is
// typically a boot-time operation; the hot side is the dispatch methods.
type Registry struct {
	mu         sync.RWMutex
	preFlight  []PreFlightMiddleware
	hooks      []Hook
	collectors []Collector
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

// RegisterHook appends h to the producer set. Nil hooks are skipped.
func (r *Registry) RegisterHook(h Hook) {
	if h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, h)
}

// RegisterCollector appends c to the janitor set. Nil collectors are
// skipped.
func (r *Registry) RegisterCollector(c Collector) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, c)
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

// Finalize runs the end-of-lifecycle sweep: every Hook fills its result
// and the Registry attaches it to lc under the hook's name (serial — the
// Registry is the sole writer of the collected set), then every
// Collector stores the collected results to its sink (parallel,
// read-only). Panics in a hook or collector are recovered and logged;
// siblings proceed.
//
// Called from the runner's detached post-flight goroutine. Blocks only
// that goroutine, never the caller — the wait gives accurate post-flight
// latency and lets graceful shutdown drain in-flight collectors.
func (r *Registry) Finalize(ctx context.Context, lc *Context, ev *PostFlightEvent) {
	r.mu.RLock()
	hooks := r.hooks
	collectors := r.collectors
	r.mu.RUnlock()

	// Produce → attach. Serial: the Registry is the only writer of the
	// collected set, so hooks can't race it.
	for _, h := range hooks {
		v := safeFill(h, lc, ev)
		if v != nil {
			lc.attach(h.Name(), v)
		}
	}

	// Store. Parallel, read-only on the collected set.
	if len(collectors) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, c := range collectors {
		wg.Add(1)
		go func(col Collector) {
			defer wg.Done()
			defer recoverLifecycle(lc, "collector")
			col.Collect(lc)
		}(c)
	}
	wg.Wait()
}

// safeFill runs one hook's Fill with panic recovery. A panicking or
// erroring hook contributes nothing; siblings proceed.
func safeFill(h Hook, lc *Context, ev *PostFlightEvent) (out any) {
	defer func() {
		if v := recover(); v != nil {
			slog.Error("lifecycle: hook Fill panic recovered",
				"hook", h.Name(), "request_id", lc.RequestID, "panic", v)
			out = nil
		}
	}()
	v, err := h.Fill(lc, ev)
	if err != nil {
		slog.Warn("lifecycle: hook Fill error",
			"hook", h.Name(), "request_id", lc.RequestID, "err", err)
		return nil
	}
	return v
}

// recoverLifecycle isolates panics inside one collector from siblings and
// the post-flight goroutine. A panicking collector is a bug; we log and
// continue. Never propagated — observers must not crash the runner.
func recoverLifecycle(lc *Context, kind string) {
	if v := recover(); v != nil {
		slog.Error("lifecycle: "+kind+" panic recovered",
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

// HookCount returns the number of registered producer hooks.
func (r *Registry) HookCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hooks)
}

// CollectorCount returns the number of registered collectors.
func (r *Registry) CollectorCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.collectors)
}
