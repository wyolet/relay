package lifecycle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- registration ---

func TestRegisterNilSkipped(t *testing.T) {
	r := New()
	r.RegisterPreFlight(nil)
	r.RegisterHook(nil)
	r.RegisterCollector(nil)
	if r.PreFlightCount() != 0 || r.HookCount() != 0 || r.CollectorCount() != 0 {
		t.Fatalf("nil registrations should not be stored; counts: pre=%d hooks=%d collectors=%d",
			r.PreFlightCount(), r.HookCount(), r.CollectorCount())
	}
}

func TestEmptyRegistryNoPanic(t *testing.T) {
	r := New()
	lc := NewContext("req-1", "test", time.Now())
	if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != nil {
		t.Fatalf("empty pre-flight: %v", err)
	}
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
}

// collectorFn adapts a func to the Collector interface for tests.
type collectorFn func(*Context)

func (f collectorFn) Collect(lc *Context) { f(lc) }

// hookFn is a Hook whose Fill runs fn and attaches its return.
func hookFn(name string, fn func(lc *Context, ev *PostFlightEvent) (any, error)) Hook {
	return HookFunc{HookName: name, Fn: fn}
}

// --- pre-flight semantics ---

func TestPreFlightSequentialOrder(t *testing.T) {
	r := New()
	var order []string
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "a")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "b")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "c")
		return nil
	})

	if err := r.RunPreFlight(context.Background(), NewContext("r", "test", time.Now()), &PreFlightEvent{}); err != nil {
		t.Fatalf("pre-flight: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !equalStrings(order, want) {
		t.Fatalf("order: got %v want %v", order, want)
	}
}

func TestPreFlightAbortOnError(t *testing.T) {
	r := New()
	abort := errors.New("budget exceeded")
	var ran []string
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "a")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "b")
		return abort
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "c") // must not run
		return nil
	})

	err := r.RunPreFlight(context.Background(), NewContext("r", "test", time.Now()), &PreFlightEvent{})
	if !errors.Is(err, abort) {
		t.Fatalf("expected abort error to propagate, got %v", err)
	}
	if !equalStrings(ran, []string{"a", "b"}) {
		t.Fatalf("expected [a b], got %v", ran)
	}
}

func TestPreFlightMutatesContext(t *testing.T) {
	r := New()
	r.RegisterPreFlight(func(_ context.Context, lc *Context, _ *PreFlightEvent) error {
		lc.PolicyID = "pol-123"
		lc.Metadata["auth_user"] = "alice"
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, lc *Context, _ *PreFlightEvent) error {
		// Subsequent middleware sees prior mutations.
		if lc.PolicyID != "pol-123" {
			t.Errorf("expected PolicyID set by prior middleware, got %q", lc.PolicyID)
		}
		if lc.Metadata["auth_user"] != "alice" {
			t.Errorf("expected metadata from prior middleware, got %v", lc.Metadata)
		}
		return nil
	})

	lc := NewContext("r", "test", time.Now())
	if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != nil {
		t.Fatalf("pre-flight: %v", err)
	}
}

// --- finalize semantics ---

func TestFinalizeAllHooksFillAndAttach(t *testing.T) {
	r := New()
	for i, name := range []string{"a", "b", "c"} {
		val := i + 1
		r.RegisterHook(hookFn(name, func(_ *Context, _ *PostFlightEvent) (any, error) {
			return val, nil
		}))
	}
	lc := NewContext("r", "test", time.Now())
	r.Finalize(context.Background(), lc, &PostFlightEvent{})

	for _, name := range []string{"a", "b", "c"} {
		if _, ok := lc.Collected(name); !ok {
			t.Fatalf("hook %q result not attached to context", name)
		}
	}
}

func TestFinalizeNilResultNotAttached(t *testing.T) {
	r := New()
	r.RegisterHook(hookFn("none", func(_ *Context, _ *PostFlightEvent) (any, error) {
		return nil, nil // nothing to contribute
	}))
	lc := NewContext("r", "test", time.Now())
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
	if _, ok := lc.Collected("none"); ok {
		t.Fatal("nil hook result should not be attached")
	}
}

func TestFinalizeCollectorsParallel(t *testing.T) {
	// Collectors run in parallel: 5 sleepers should finish in ≈sleep, not
	// 5*sleep. Generous margin to avoid CI flake.
	r := New()
	const n = 5
	const sleep = 50 * time.Millisecond
	for i := 0; i < n; i++ {
		r.RegisterCollector(collectorFn(func(_ *Context) { time.Sleep(sleep) }))
	}
	start := time.Now()
	r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if elapsed := time.Since(start); elapsed > 3*sleep {
		t.Fatalf("collectors not parallel: elapsed=%v, sequential≈%v", elapsed, n*sleep)
	}
}

func TestFinalizePanicIsolated(t *testing.T) {
	r := New()
	var hookRan, collRan int64

	r.RegisterHook(hookFn("ok1", func(_ *Context, _ *PostFlightEvent) (any, error) {
		atomic.AddInt64(&hookRan, 1)
		return 1, nil
	}))
	r.RegisterHook(hookFn("boom", func(_ *Context, _ *PostFlightEvent) (any, error) {
		panic("intentional in test")
	}))
	r.RegisterHook(hookFn("ok2", func(_ *Context, _ *PostFlightEvent) (any, error) {
		atomic.AddInt64(&hookRan, 1)
		return 2, nil
	}))
	r.RegisterCollector(collectorFn(func(_ *Context) { atomic.AddInt64(&collRan, 1) }))
	r.RegisterCollector(collectorFn(func(_ *Context) { panic("intentional in test") }))
	r.RegisterCollector(collectorFn(func(_ *Context) { atomic.AddInt64(&collRan, 1) }))

	// Must not panic out of Finalize; non-panicking hooks + collectors run.
	r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if got := atomic.LoadInt64(&hookRan); got != 2 {
		t.Fatalf("expected 2 non-panicking hooks, got %d", got)
	}
	if got := atomic.LoadInt64(&collRan); got != 2 {
		t.Fatalf("expected 2 non-panicking collectors, got %d", got)
	}
}

func TestFinalizeHookSeesContextAndEvent(t *testing.T) {
	r := New()
	wantID, wantStatus := "req-shared", 200
	var gotID string
	var gotStatus int
	r.RegisterHook(hookFn("probe", func(lc *Context, ev *PostFlightEvent) (any, error) {
		gotID = lc.RequestID
		gotStatus = ev.Status
		return nil, nil
	}))
	r.Finalize(context.Background(), NewContext(wantID, "test", time.Now()), &PostFlightEvent{Status: wantStatus})
	if gotID != wantID || gotStatus != wantStatus {
		t.Fatalf("hook did not see lc/ev: id=%q status=%d", gotID, gotStatus)
	}
}

func TestCollectorReadsHookResult(t *testing.T) {
	r := New()
	r.RegisterHook(hookFn("usage", func(_ *Context, _ *PostFlightEvent) (any, error) {
		return "the-result", nil
	}))
	var seen string
	r.RegisterCollector(collectorFn(func(lc *Context) {
		if v, ok := lc.Collected("usage"); ok {
			seen, _ = v.(string)
		}
	}))
	r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if seen != "the-result" {
		t.Fatalf("collector did not read hook result off context: %q", seen)
	}
}

// --- stream observers ---

type testStreamFactory struct {
	name string
	obs  *testStreamObserver
}

func (f *testStreamFactory) Name() string { return f.name }
func (f *testStreamFactory) NewObserver(_ *Context) StreamObserver {
	f.obs = &testStreamObserver{}
	return f.obs
}

type testStreamObserver struct {
	frames int
}

func (o *testStreamObserver) Observe(_ []byte)     { o.frames++ }
func (o *testStreamObserver) Result() (any, error) { return o.frames, nil }

func TestStreamSession_ObserveFinishAttachesAndFills(t *testing.T) {
	r := New()
	f := &testStreamFactory{name: "frames"}
	r.RegisterStreamObserver(f)

	lc := NewContext("r", "test", time.Now())
	sess := r.NewStreamSession(lc)
	if sess == nil {
		t.Fatal("expected a session for a registered factory")
	}
	sess.Observe([]byte("a"))
	sess.Observe([]byte("b"))
	sess.Finish()

	if v, ok := lc.Collected("frames"); !ok || v.(int) != 2 {
		t.Fatalf("stream observer result not attached: %v ok=%v", v, ok)
	}
	if !lc.filled {
		t.Fatal("Finish must mark the context filled")
	}

	// A subsequent post-flight Finalize must NOT re-run hooks (collect-once):
	// register a hook that would panic-mark if it ran.
	ran := false
	r.RegisterHook(hookFn("h", func(_ *Context, _ *PostFlightEvent) (any, error) {
		ran = true
		return 1, nil
	}))
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
	if ran {
		t.Fatal("Finalize re-ran hooks despite stream session already filling")
	}
}

func TestNewStreamSession_NilWhenNoFactories(t *testing.T) {
	r := New()
	if s := r.NewStreamSession(NewContext("r", "test", time.Now())); s != nil {
		t.Fatal("expected nil session with no factories")
	}
}

// --- concurrent register / finalize ---

func TestConcurrentRegisterAndFinalize(t *testing.T) {
	// race detector catches data races on the slices + collected map
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RegisterHook(hookFn("h", func(_ *Context, _ *PostFlightEvent) (any, error) { return 1, nil }))
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
		}()
	}
	wg.Wait()
	if r.HookCount() != 50 {
		t.Fatalf("expected 50 registered, got %d", r.HookCount())
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
