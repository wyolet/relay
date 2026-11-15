package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
)

type collectorFn func(*Context)

func (f collectorFn) Collect(lc *Context) { f(lc) }

func hookFn(name string, fn func(lc *Context, ev *PostFlightEvent) (any, error)) Hook {
	return HookFunc{HookName: name, Fn: fn}
}

// collected reads the named results off lc; an absent name is left out.
func collected(lc *Context, names ...string) map[string]any {
	out := map[string]any{}
	for _, n := range names {
		if v, ok := lc.Collected(n); ok {
			out[n] = v
		}
	}
	return out
}

// goroutineID parses the current goroutine's id from its stack header ("goroutine N [running]:").
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	id, _ := strconv.ParseUint(string(buf[:bytes.IndexByte(buf, ' ')]), 10, 64)
	return id
}

func TestRegisterSkipsNil(t *testing.T) {
	r := New()
	r.RegisterPreFlight(nil)
	r.RegisterHook(nil)
	r.RegisterCollector(nil)
	r.RegisterStreamObserver(nil)
	if got := [4]int{r.PreFlightCount(), r.HookCount(), r.CollectorCount(), r.StreamObserverCount()}; got != [4]int{} {
		t.Fatalf("counts after nil registrations (pre-flight, hooks, collectors, stream observers) = %v, want all 0", got)
	}
	r.RegisterPreFlight(func(context.Context, *Context, *PreFlightEvent) error { return nil })
	r.RegisterHook(hookFn("h", nil))
	r.RegisterHook(hookFn("h2", nil))
	r.RegisterCollector(collectorFn(nil))
	r.RegisterStreamObserver(&recordingFactory{})
	if got := [4]int{r.PreFlightCount(), r.HookCount(), r.CollectorCount(), r.StreamObserverCount()}; got != [4]int{1, 2, 1, 1} {
		t.Fatalf("counts = %v, want [1 2 1 1]", got)
	}
}

func TestEmptyRegistryRunsNothing(t *testing.T) {
	r := New()
	lc := NewContext("req-1", "test", time.Now())
	if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != nil {
		t.Fatalf("empty pre-flight: %v", err)
	}
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
	r.Fill(nil, &PostFlightEvent{})
	if s := r.NewStreamSession(lc); s != nil {
		t.Fatal("a stream session without stream observers")
	}
}

// Middlewares run in registration order, see each other's changes, and stop at the first error.
func TestRunPreFlight(t *testing.T) {
	abort := errors.New("budget exceeded")
	for _, tc := range []struct {
		name    string
		failAt  string
		wantRan []string
		wantErr error
	}{
		{"all pass", "", []string{"a", "b", "c"}, nil},
		{"the first error aborts the chain", "b", []string{"a", "b"}, abort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			var ran []string
			for _, name := range []string{"a", "b", "c"} {
				r.RegisterPreFlight(func(_ context.Context, lc *Context, _ *PreFlightEvent) error {
					ran = append(ran, name)
					seen, _ := lc.Metadata["seen"].(string)
					lc.Metadata["seen"] = seen + name
					lc.PolicyID = "set-by-" + name
					if name == tc.failAt {
						return abort
					}
					return nil
				})
			}
			lc := NewContext("r", "test", time.Now())
			if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.wantRan, ran); diff != "" {
				t.Errorf("middlewares run (-want +got):\n%s", diff)
			}
			last := tc.wantRan[len(tc.wantRan)-1]
			if lc.Metadata["seen"] != strings.Join(tc.wantRan, "") || lc.PolicyID != "set-by-"+last {
				t.Errorf("context after the chain: seen %v, policy %q", lc.Metadata["seen"], lc.PolicyID)
			}
		})
	}
}

// Each hook's value is attached under its name; a nil value, an error and a panic attach nothing and stop no other hook.
func TestFinalizeAttachesHookResults(t *testing.T) {
	r := New()
	ev := &PostFlightEvent{Status: 207}
	var sawID string
	var sawEvent *PostFlightEvent
	r.RegisterHook(hookFn("first", func(lc *Context, ev *PostFlightEvent) (any, error) {
		sawID, sawEvent = lc.RequestID, ev
		return 1, nil
	}))
	r.RegisterHook(hookFn("nothing", func(*Context, *PostFlightEvent) (any, error) { return nil, nil }))
	r.RegisterHook(hookFn("failed", func(*Context, *PostFlightEvent) (any, error) { return "partial", errors.New("no usage block") }))
	r.RegisterHook(hookFn("panicked", func(*Context, *PostFlightEvent) (any, error) { panic("intentional in test") }))
	r.RegisterHook(hookFn("last", func(*Context, *PostFlightEvent) (any, error) { return []string{"end"}, nil }))
	var atCollect map[string]any
	r.RegisterCollector(collectorFn(func(lc *Context) {
		atCollect = collected(lc, "first", "nothing", "failed", "panicked", "last")
	}))

	lc := NewContext("req-7", "test", time.Now())
	r.Finalize(context.Background(), lc, ev)
	want := map[string]any{"first": 1, "last": []string{"end"}}
	if diff := cmp.Diff(want, atCollect); diff != "" {
		t.Errorf("results a collector reads (-want +got):\n%s", diff)
	}
	if sawID != "req-7" || sawEvent != ev {
		t.Errorf("hook saw request %q and event %p, want req-7 and the event passed to Finalize", sawID, sawEvent)
	}
}

// Fill before the response is written is not repeated by Finalize.
func TestFillRunsHooksOnce(t *testing.T) {
	r := New()
	runs := 0
	r.RegisterHook(hookFn("usage", func(*Context, *PostFlightEvent) (any, error) {
		runs++
		return runs, nil
	}))
	collects := 0
	r.RegisterCollector(collectorFn(func(*Context) { collects++ }))
	lc := NewContext("r", "test", time.Now())

	r.Fill(lc, &PostFlightEvent{})
	if v, _ := lc.Collected("usage"); v != 1 || collects != 0 {
		t.Fatalf("after Fill: usage = %v, %d collects; want 1 and no collect", v, collects)
	}
	r.Fill(lc, &PostFlightEvent{})
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
	if v, _ := lc.Collected("usage"); v != 1 || runs != 1 || collects != 1 {
		t.Fatalf("after Finalize: usage = %v, hook ran %d times, %d collects; want 1, 1, 1", v, runs, collects)
	}

	// The next request fills afresh.
	other := NewContext("r2", "test", time.Now())
	r.Finalize(context.Background(), other, &PostFlightEvent{})
	if v, _ := other.Collected("usage"); v != 2 {
		t.Fatalf("second request: usage = %v, want 2", v)
	}
}

// Collectors run in registration order on the caller's goroutine; one that panics does not stop the ones after it.
func TestFinalizeRunsCollectorsInOrderOnTheCallingGoroutine(t *testing.T) {
	r := New()
	caller := goroutineID()
	var order []string
	for _, name := range []string{"a", "b", "boom", "c"} {
		r.RegisterCollector(collectorFn(func(*Context) {
			if id := goroutineID(); id != caller {
				t.Errorf("collector %q ran on goroutine %d, want caller %d", name, id, caller)
			}
			order = append(order, name)
			if name == "boom" {
				panic("intentional in test")
			}
		}))
	}
	r.Finalize(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if diff := cmp.Diff([]string{"a", "b", "boom", "c"}, order); diff != "" {
		t.Fatalf("collectors run (-want +got):\n%s", diff)
	}
}

// The observer gets the whole pass: hooks and collectors, panics included.
func TestFinalizeObserverTimesTheWholePass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := New()
		r.RegisterHook(hookFn("slow", func(*Context, *PostFlightEvent) (any, error) {
			time.Sleep(2 * time.Millisecond)
			return nil, nil
		}))
		r.RegisterCollector(collectorFn(func(*Context) {
			time.Sleep(3 * time.Millisecond)
			panic("intentional in test")
		}))
		var got []time.Duration
		r.SetFinalizeObserver(func(d time.Duration) { got = append(got, d) })
		lc := NewContext("r", "test", time.Now())
		r.Finalize(context.Background(), lc, &PostFlightEvent{})
		// The hooks already ran for this request, so the second pass is the collector alone.
		r.Finalize(context.Background(), lc, &PostFlightEvent{})
		if diff := cmp.Diff([]time.Duration{5 * time.Millisecond, 3 * time.Millisecond}, got); diff != "" {
			t.Errorf("observed durations (-want +got):\n%s", diff)
		}
		r.SetFinalizeObserver(nil)
		r.Finalize(context.Background(), lc, &PostFlightEvent{})
		if len(got) != 2 {
			t.Errorf("a cleared observer was called: %v", got)
		}
	})
}

// Registration during traffic is safe: a request attaches each hook it ran under that hook's own name.
func TestConcurrentRegisterAndFinalize(t *testing.T) {
	r := New()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			r.RegisterHook(hookFn("h"+strconv.Itoa(i), func(*Context, *PostFlightEvent) (any, error) { return i, nil }))
		})
		wg.Go(func() {
			lc := NewContext("r", "test", time.Now())
			r.Finalize(context.Background(), lc, &PostFlightEvent{})
			for j := range n {
				if v, ok := lc.Collected("h" + strconv.Itoa(j)); ok {
					if v != j {
						t.Errorf("hook h%d attached %v", j, v)
					}
				}
			}
		})
	}
	wg.Wait()
	if r.HookCount() != n {
		t.Fatalf("HookCount = %d, want %d", r.HookCount(), n)
	}
	lc := NewContext("r", "test", time.Now())
	r.Finalize(context.Background(), lc, &PostFlightEvent{})
	for j := range n {
		if v, _ := lc.Collected("h" + strconv.Itoa(j)); v != j {
			t.Fatalf("after all registrations, h%d = %v", j, v)
		}
	}
}

func TestContextCarriers(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lc := NewContext("req-1", "proxy", start)
	if lc.RequestID != "req-1" || lc.Source != "proxy" || !lc.Timing.Start.Equal(start) || lc.Metadata == nil {
		t.Fatalf("NewContext = %+v", lc)
	}
	if got := FromContext(ContextWith(context.Background(), lc)); got != lc {
		t.Errorf("FromContext = %p, want the context stored", got)
	}
	if got := FromContext(context.Background()); got != nil {
		t.Errorf("FromContext of a bare context = %p, want nil", got)
	}

	// Every accessor is safe on a nil Context and reports nothing.
	var none *Context
	none.attach("x", 1)
	none.SetStreamSession(&StreamSession{})
	none.SetResponseSummary(lc.responseSummary)
	none.setStreamTokens(map[string]int64{"input": 1})
	if _, ok := none.Collected("x"); ok {
		t.Error("nil Context reports a collected value")
	}
	if _, ok := none.StreamTokens(); ok {
		t.Error("nil Context reports stream tokens")
	}
	if _, ok := none.StreamSummary(); ok {
		t.Error("nil Context reports a stream summary")
	}
	if _, ok := none.ResponseSummary(); ok {
		t.Error("nil Context reports a response summary")
	}
	if none.StreamSession() != nil {
		t.Error("nil Context reports a stream session")
	}

	if _, ok := lc.ResponseSummary(); ok {
		t.Error("a response summary before one was set")
	}
	lc.SetResponseSummary(lc.responseSummary)
	if _, ok := lc.ResponseSummary(); !ok {
		t.Error("no response summary after SetResponseSummary")
	}
	sess := &StreamSession{}
	lc.SetStreamSession(sess)
	if lc.StreamSession() != sess {
		t.Error("StreamSession did not return the session set")
	}
	lc.attach("nil-value", nil)
	if _, ok := lc.Collected("nil-value"); ok {
		t.Error("a nil value was attached")
	}
}
