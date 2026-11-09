package httpapi

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

const testTarget = 5 * time.Millisecond

// holdN admits n requests and returns their contexts.
func holdN(t *testing.T, a *Admission, n int) []*lifecycle.Context {
	t.Helper()
	held := make([]*lifecycle.Context, n)
	for i := range held {
		held[i] = newLC("held")
		if err := admit(a, held[i]); err != nil {
			t.Fatalf("admit %d of %d: %v", i+1, n, err)
		}
	}
	return held
}

func TestAdjustLimit(t *testing.T) {
	overloaded, healthy := 20*time.Millisecond, time.Millisecond
	for _, tc := range []struct {
		name        string
		hard, limit int64
		inflight    int
		p99         time.Duration
		samples     uint64
		breaches    int
		want        int64
	}{
		{"overloaded drops to 90% of in flight", 2048, 2048, 1000, overloaded, 500, breachWindows - 1, 900},
		{"overloaded drops to 90% of the limit when it is lower", 2048, 500, 800, overloaded, 500, breachWindows - 1, 450},
		{"overloaded stops at the floor", 2048, 2048, 10, overloaded, 500, breachWindows - 1, adaptiveFloor},
		{"floor never exceeds the hard cap", 10, 10, 5, overloaded, 500, breachWindows - 1, 10},
		{"too few samples count as healthy", 2048, 900, 900, time.Second, minSchedSamples - 1, 0, 990},
		{"p99 at the target is healthy", 2048, 900, 0, testTarget, 500, 0, 990},
		{"healthy grows by a tenth", 2048, 900, 0, healthy, 500, 0, 990},
		{"healthy grows by at least the minimum step", 2048, 64, 0, healthy, 500, 0, 64 + adaptiveMinStep},
		{"a breach shorter than breachWindows holds the limit", 2048, 900, 900, overloaded, 500, breachWindows - 2, 900},
		{"a healthy window resets the breach and grows", 2048, 900, 0, healthy, 500, breachWindows - 1, 990},
		{"healthy stops at the hard cap", 2048, 2040, 0, healthy, 500, 0, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdmission(int(tc.hard))
			a.limit.Store(tc.hard)
			holdN(t, a, tc.inflight)
			a.limit.Store(tc.limit)
			a.breaches = tc.breaches
			a.adjustLimit(Load{SchedP99: tc.p99, Samples: tc.samples}, testTarget)
			if got := a.limit.Load(); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
			if got := testutil.ToFloat64(metrics.InflightAdaptiveLimit); got != float64(tc.want) {
				t.Fatalf("adaptive limit gauge = %v, want %d", got, tc.want)
			}
		})
	}
}

// Both stages shed at the effective limit, below the hard cap, and a release lets the next request in.
func TestAdmission_ShedsAtEffectiveLimit(t *testing.T) {
	a := NewAdmission(100)
	a.limit.Store(2)
	held := holdN(t, a, 2)
	if !a.Full() {
		t.Fatal("not full at the effective limit")
	}
	if err := admit(a, newLC("over")); !errors.Is(err, ErrShed) {
		t.Fatalf("admit past the effective limit err = %v, want ErrShed", err)
	}
	a.Collect(held[0])
	if a.Full() {
		t.Fatal("still full after a release")
	}
	if err := admit(a, newLC("after release")); err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	if got := len(a.sem); got != 2 {
		t.Fatalf("slots held = %d, want 2", got)
	}
}

// A limit lowered below what is in flight sheds every new request until enough releases bring the count under it.
func TestAdmission_LimitBelowInflight(t *testing.T) {
	a := NewAdmission(100)
	held := holdN(t, a, 10)
	a.limit.Store(5)
	for i := range 5 {
		if err := admit(a, newLC("over")); !errors.Is(err, ErrShed) {
			t.Fatalf("admit with %d in flight err = %v, want ErrShed", 10-i, err)
		}
		a.Collect(held[i])
	}
	if !a.Full() {
		t.Fatal("not full with 5 in flight at limit 5")
	}
	a.Collect(held[5])
	if err := admit(a, newLC("under")); err != nil {
		t.Fatalf("admit with 4 in flight at limit 5: %v", err)
	}
}

// AdaptLimit acts on the injected source each interval and returns when its context ends.
func TestAdaptLimit_RunsUntilContextEnds(t *testing.T) {
	a := NewAdmission(1000)
	var mu sync.Mutex
	p99 := 50 * time.Millisecond
	load := func() Load {
		mu.Lock()
		defer mu.Unlock()
		return Load{SchedP99: p99, Samples: 1000}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.AdaptLimit(ctx, testTarget, load); close(done) }()

	waitFor(t, func() bool { return a.Limit() == adaptiveFloor }, "limit at the floor while overloaded")
	if got := testutil.ToFloat64(metrics.SchedLatencyP99Seconds); got != 0.05 {
		t.Fatalf("p99 gauge = %v, want 0.05", got)
	}
	mu.Lock()
	p99 = time.Millisecond
	mu.Unlock()
	waitFor(t, func() bool { return a.Limit() == 1000 }, "limit back at the hard cap once healthy")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AdaptLimit did not return after its context ended")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHistogramP99(t *testing.T) {
	buckets := []float64{math.Inf(-1), 0.001, 0.002, 0.004, math.Inf(1)}
	for _, tc := range []struct {
		name        string
		counts      []uint64
		want        time.Duration
		wantSamples uint64
	}{
		{"empty", []uint64{0, 0, 0, 0}, 0, 0},
		{"all in one bucket", []uint64{0, 0, 100, 0}, 4 * time.Millisecond, 100},
		{"p99 in the tail", []uint64{0, 98, 2, 0}, 4 * time.Millisecond, 100},
		{"tail at 1%", []uint64{0, 99, 1, 0}, 2 * time.Millisecond, 100},
		{"tail under 1%", []uint64{0, 999, 1, 0}, 2 * time.Millisecond, 1000},
		{"tail well under 1%", []uint64{0, 9990, 10, 0}, 2 * time.Millisecond, 10000},
		{"overflow bucket reports its lower bound", []uint64{0, 0, 0, 5}, 4 * time.Millisecond, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := histogramP99(tc.counts, buckets)
			if got != tc.want || n != tc.wantSamples {
				t.Fatalf("histogramP99 = %v, %d; want %v, %d", got, n, tc.want, tc.wantSamples)
			}
		})
	}
}

// The runtime source counts only scheduling since its previous read.
func TestRuntimeSchedLatency(t *testing.T) {
	sched := RuntimeSchedLatency()
	var wg sync.WaitGroup
	for range 2000 {
		wg.Go(runtime.Gosched)
	}
	wg.Wait()
	_, first := sched()
	if first == 0 {
		t.Fatal("no scheduler samples after 2000 goroutines ran")
	}
	if _, again := sched(); again >= first {
		t.Fatalf("second read counted %d samples, want fewer than the first read's %d", again, first)
	}
}

// CPU share at or past cpuBusyShare is overload even when the scheduler looks calm; below it, a calm scheduler is healthy.
func TestAdjustLimit_CPUShare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		share float64
		want  int64
	}{
		{"busy", cpuBusyShare, 900},
		{"below busy", cpuBusyShare - 0.05, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdmission(2048)
			holdN(t, a, 1000)
			a.breaches = breachWindows - 1
			a.adjustLimit(Load{CPUShare: tc.share}, testTarget)
			if got := a.limit.Load(); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRuntimeLoad_CPUShare(t *testing.T) {
	load := RuntimeLoad()
	for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline); {
	}
	w := load()
	if w.CPUShare <= 0 || w.CPUShare > 1.5 {
		t.Fatalf("CPU share after a busy loop = %v, want in (0, 1.5]", w.CPUShare)
	}
}
