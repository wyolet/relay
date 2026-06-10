package lifecycle

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Registry holds the registered pre-flight middleware, post-flight Hooks
// (producers), and Collectors (the janitors that store). Construct one at
// the composition root, register during boot, pass into every
// request-runner.
//
// Safe for concurrent Register* / Run* / Finalize calls. Registration is
// typically a boot-time operation; the hot side is the dispatch methods.
type Registry struct {
	mu               sync.RWMutex
	preFlight        []PreFlightMiddleware
	hooks            []Hook
	collectors       []Collector
	streamFactories  []StreamObserverFactory
	finalizeObserver func(time.Duration)
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

// SetFinalizeObserver installs fn to receive the wall-clock duration of
// each Finalize fan-out. A callback rather than a metric so this package
// stays decoupled from any metrics backend — the composition root wires
// the histogram. Last call wins; nil clears.
func (r *Registry) SetFinalizeObserver(fn func(time.Duration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeObserver = fn
}

// RegisterStreamObserver appends f to the stream-observer factory set.
// Nil factories are skipped.
func (r *Registry) RegisterStreamObserver(f StreamObserverFactory) {
	if f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamFactories = append(r.streamFactories, f)
}

// --- dispatch ---

// NewStreamSession builds a fresh observer per registered factory for one
// streamed request. Returns nil when no factories are registered (the
// caller skips driving a session). Feed each upstream frame to Observe,
// then call Finish at end-of-stream.
func (r *Registry) NewStreamSession(lc *Context) *StreamSession {
	r.mu.RLock()
	facs := r.streamFactories
	r.mu.RUnlock()
	if len(facs) == 0 || lc == nil {
		return nil
	}
	obs := make([]namedObserver, 0, len(facs))
	for _, f := range facs {
		obs = append(obs, namedObserver{name: f.Name(), o: f.NewObserver(lc)})
	}
	return &StreamSession{lc: lc, obs: obs}
}

// StreamSession drives the per-request stream observers for one streamed
// response. Not safe for concurrent use — one stream, one goroutine.
type StreamSession struct {
	lc  *Context
	obs []namedObserver
}

type namedObserver struct {
	name string
	o    StreamObserver
}

// Observe feeds one upstream frame to every observer. Nil-safe so callers
// can hold a nil *StreamSession (no factories) and call unconditionally.
func (s *StreamSession) Observe(frame []byte) {
	if s == nil {
		return
	}
	for _, no := range s.obs {
		func() {
			defer recoverLifecycle(s.lc, "stream observer Observe")
			no.o.Observe(frame)
		}()
	}
}

// Finish closes the session: each observer's Result is attached to the
// Context under its name (the Registry is still the sole writer), and the
// request is marked filled so the post-flight Finalize reuses the same
// collection instead of re-producing it. Nil-safe.
func (s *StreamSession) Finish() {
	if s == nil || s.lc == nil {
		return
	}
	for _, no := range s.obs {
		v := safeResult(no, s.lc)
		if v != nil {
			s.lc.attach(no.name, v)
		}
	}
	s.lc.filled = true
}

func safeResult(no namedObserver, lc *Context) (out any) {
	defer func() {
		if v := recover(); v != nil {
			slog.Error("lifecycle: stream observer Result panic recovered",
				"observer", no.name, "request_id", lc.RequestID, "panic", v)
			out = nil
		}
	}()
	v, err := no.o.Result()
	if err != nil {
		slog.Warn("lifecycle: stream observer Result error",
			"observer", no.name, "request_id", lc.RequestID, "err", err)
		return nil
	}
	return v
}

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

// Fill runs every Hook and attaches its result to lc, serially (the
// Registry is the sole writer of the collected set). Idempotent: a second
// call is a no-op, so a pre-send fill (usage echo, which needs the
// collected results before the response is written) isn't repeated by the
// post-send Finalize. Nil-safe on lc.
func (r *Registry) Fill(lc *Context, ev *PostFlightEvent) {
	if lc == nil || lc.filled {
		return
	}
	r.mu.RLock()
	hooks := r.hooks
	r.mu.RUnlock()
	for _, h := range hooks {
		if v := safeFill(h, lc, ev); v != nil {
			lc.attach(h.Name(), v)
		}
	}
	lc.filled = true
}

// Finalize runs the end-of-lifecycle sweep: Fill (if not already done
// pre-send) then every Collector stores the collected results to its sink
// (parallel, read-only). Panics in a hook or collector are recovered and
// logged; siblings proceed.
//
// Called from the runner's detached post-flight goroutine. Blocks only
// that goroutine, never the caller — the wait gives accurate post-flight
// latency and lets graceful shutdown drain in-flight collectors.
func (r *Registry) Finalize(ctx context.Context, lc *Context, ev *PostFlightEvent) {
	r.mu.RLock()
	obs := r.finalizeObserver
	r.mu.RUnlock()
	if obs != nil {
		start := time.Now()
		defer func() { obs(time.Since(start)) }()
	}

	r.Fill(lc, ev)

	r.mu.RLock()
	collectors := r.collectors
	r.mu.RUnlock()

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

// StreamObserverCount returns the number of registered stream-observer
// factories.
func (r *Registry) StreamObserverCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.streamFactories)
}
