package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func TestFinalizeObserverSeesFanOutDuration(t *testing.T) {
	r := New()
	r.RegisterHook(hookFn("slow", func(_ *Context, _ *PostFlightEvent) (any, error) {
		time.Sleep(2 * time.Millisecond)
		return nil, nil
	}))
	var calls int
	var got time.Duration
	r.SetFinalizeObserver(func(d time.Duration) { calls++; got = d })
	r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if calls != 1 {
		t.Fatalf("observer calls = %d, want 1", calls)
	}
	if got < 2*time.Millisecond {
		t.Fatalf("observer duration = %v, want >= 2ms (the fan-out time)", got)
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

// The tee path: raw upstream bytes written to the session (io.Writer) are
// reframed on the SSE "\n\n" separator and delivered to observers as whole
// frames — across arbitrary Write boundaries — with only the trailing
// partial frame flushed at Finish.
func TestStreamSession_WriteReframesAcrossBoundaries(t *testing.T) {
	r := New()
	f := &recordingFactory{}
	r.RegisterStreamObserver(f)

	sess := r.NewStreamSession(NewContext("r", "test", time.Now()))
	// Two complete frames + a trailing partial, split mid-frame across Writes.
	io.WriteString(sess, "event: a\ndata: 1\n\nev")
	io.WriteString(sess, "ent: b\ndata: 2\n\ndata: tail-no-sep")
	sess.Finish()

	want := []string{"event: a\ndata: 1", "event: b\ndata: 2", "data: tail-no-sep"}
	if len(f.obs.frames) != len(want) {
		t.Fatalf("frames: got %q want %q", f.obs.frames, want)
	}
	for i, w := range want {
		if f.obs.frames[i] != w {
			t.Fatalf("frame %d: got %q want %q", i, f.obs.frames[i], w)
		}
	}
}

// Finish is idempotent (echo finishes early, then the runner finishes again)
// and stashes the summarizer's tokens for the runner. With no translator the
// summarizer yields nothing, but StreamTokens must still report "extracted".
func TestStreamSession_FinishIdempotentStashesTokens(t *testing.T) {
	r := New()
	f := &recordingFactory{}
	r.RegisterStreamObserver(f)

	sess := r.NewStreamSession(NewContext("r", "test", time.Now()))
	lc := sess.lc
	io.WriteString(sess, "event: a\ndata: 1\n\n")
	sess.Finish()
	sess.Finish() // no-op

	if got := f.obs.results; got != 1 {
		t.Fatalf("Result called %d times, want 1 (idempotent Finish)", got)
	}
	if _, ok := lc.StreamTokens(); !ok {
		t.Fatal("Finish must mark stream tokens extracted")
	}
}

type recordingFactory struct{ obs *recordingObserver }

func (f *recordingFactory) Name() string { return "rec" }
func (f *recordingFactory) NewObserver(_ *Context) StreamObserver {
	f.obs = &recordingObserver{}
	return f.obs
}

type recordingObserver struct {
	frames  []string
	results int
}

func (o *recordingObserver) Observe(f []byte) { o.frames = append(o.frames, string(f)) }
func (o *recordingObserver) Result() (any, error) {
	o.results++
	return len(o.frames), nil
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

// CRLF-separated SSE (spec-legal) must reframe identically to LF, with
// frames normalized to LF before observers see them.
func TestStreamSession_WriteReframesCRLF(t *testing.T) {
	r := New()
	f := &recordingFactory{}
	r.RegisterStreamObserver(f)

	sess := r.NewStreamSession(NewContext("r", "test", time.Now()))
	io.WriteString(sess, "event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n")
	sess.Finish()

	want := []string{"event: a\ndata: 1", "event: b\ndata: 2"}
	if len(f.obs.frames) != len(want) {
		t.Fatalf("frames: got %q want %q", f.obs.frames, want)
	}
	for i, w := range want {
		if f.obs.frames[i] != w {
			t.Fatalf("frame %d: got %q want %q", i, f.obs.frames[i], w)
		}
	}
}

// Input that never yields a separator must not grow the partial buffer past
// maxPartialFrameBytes — the bound is the invariant the whole redesign rests
// on. Overflow feeds the oversized chunk best-effort and resets.
func TestStreamSession_WritePartialBufferBounded(t *testing.T) {
	r := New()
	f := &recordingFactory{}
	r.RegisterStreamObserver(f)

	sess := r.NewStreamSession(NewContext("r", "test", time.Now()))
	chunk := bytes.Repeat([]byte("x"), 256*1024) // no separator anywhere
	for i := 0; i < 8; i++ {                     // 2 MiB total
		if _, err := sess.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if len(sess.buf) > maxPartialFrameBytes {
			t.Fatalf("partial buffer grew to %d, cap is %d", len(sess.buf), maxPartialFrameBytes)
		}
	}
	if len(f.obs.frames) == 0 {
		t.Fatal("overflow chunk was never fed best-effort")
	}
}
