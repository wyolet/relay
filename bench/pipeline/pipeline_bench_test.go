// Package pipeline_bench is an in-process regression harness for
// app/pipeline.Pipeline.Run. CLAUDE.md sets two regimes:
//
//   - Live distributed (real Redis, two-pod fleet, nginx LB):
//     p50 < 2ms, p99 ≤ 15ms (public SLO). Out of scope here.
//   - In-process (in-memory kv, no network):
//     p50 < 100µs, p99 < 500µs. THAT is what this harness measures.
//
// The gap between the two is unavoidable I/O (Redis Lua RTT, nginx
// hop, container network). The in-process number is the architecture's
// lower bound — when it regresses, the live numbers regress at least
// as much.
//
// Two surfaces:
//
//   - Benchmark* funcs report ns/op + per-bench p50/p99 via
//     b.ReportMetric. Run with `go test -bench=. ./bench/pipeline/`.
//   - TestPipelinePerf_Gate is a regression test that fails when
//     observed p99 exceeds a loose multiple of the CLAUDE.md target.
//     Run by default; skipped with -short. Threshold is 2× the SLO
//     so it catches real regressions without flaking on noisy laptops.
package pipeline_bench

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// CLAUDE.md in-process SLO. Test gate uses a 2× multiplier to absorb
// laptop noise; tighten on a dedicated bench box if you want.
const (
	p50Target = 100 * time.Microsecond
	p99Target = 500 * time.Microsecond
	gateSlack = 2.0
)

// stubAdapter is a zero-IO Adapter. Call returns a pre-built response
// pointing at an in-memory body; ExtractTokens returns a fixed map;
// Retryable always says "no retry." This isolates pipeline orchestration
// overhead from upstream and adapter cost.
type stubAdapter struct {
	body   string
	tokens pkgusage.Tokens
	calls  atomic.Int64
}

func (s *stubAdapter) Call(_ context.Context, _, _ string, _ []byte, _ http.Header, _ string, _ bool) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(s.body)),
	}, nil
}

func (s *stubAdapter) ExtractTokens(_ []byte) pkgusage.Tokens { return s.tokens }

func (s *stubAdapter) Retryable(_ *http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

// fixture is the pre-resolved Request + Pipeline pair we exercise.
// withLimiter toggles whether ratelimit.Limiter is attached — measures
// "pure orchestration" vs "orchestration + kv.Mem Reserve/Commit."
type fixture struct {
	pipe *pipeline.Pipeline
	req  *pipeline.Request
}

// silentLogger drops keypool/limiter chatter that would otherwise
// drown bench output. Bench timing is the only signal we care about
// here; structured logs are observed in unit tests and real runs.
var silentLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// benchSnap is an empty SnapshotReader — bench policy has no RL, so
// the limiter is never invoked from the policy.Service path.
type benchSnap struct{}

func (benchSnap) Policy(string) (*policy.Policy, bool)              { return nil, false }
func (benchSnap) RateLimit(string) (*ratelimit.RateLimit, bool)     { return nil, false }

func newFixture(withLimiter bool) *fixture {
	kvStore := kv.NewMem()
	selector := keypool.New(kvStore, silentLogger, nil, nil)
	var limiter *pkgratelimit.Limiter
	if withLimiter {
		limiter = pkgratelimit.New(kvStore, silentLogger, nil)
	}

	svc := policy.NewService(benchSnap{}, selector, limiter)
	p := &pipeline.Pipeline{Policy: svc, Logger: silentLogger}

	pol := &policy.Policy{Spec: policy.Spec{KeySelection: policy.KeySelectionPrioritized}}
	key := &hostkey.HostKey{Resolved: "sk-bench", KeyHash: "bench-hash"}

	req := &pipeline.Request{
		Body:        []byte(`{"model":"bench","messages":[{"role":"user","content":"hi"}]}`),
		Headers:     http.Header{},
		HostBaseURL: "http://example.invalid",
		Adapter:     &stubAdapter{body: `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, tokens: pkgusage.Tokens{"input": 1, "output": 1}},
		Policy:      pol,
		Keys:        []*hostkey.HostKey{key},
		ModelName:   "bench-model",
	}
	return &fixture{pipe: p, req: req}
}

// runOnce runs Run + drains the body. Returns the synchronous wall
// time the caller observes — what users actually pay. Post-flight runs
// detached and is not counted (it's a background tax, not part of
// request latency).
func runOnce(b interface{ Fatal(args ...any) }, f *fixture) time.Duration {
	start := time.Now()
	res, err := f.pipe.Run(context.Background(), f.req)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return time.Since(start)
}

func benchPipeline(b *testing.B, withLimiter bool) {
	f := newFixture(withLimiter)
	// Warmup — the selector's first call hits a few kv writes that
	// don't recur on the steady-state path.
	for i := 0; i < 100; i++ {
		_ = runOnce(b, f)
	}

	durs := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		durs = append(durs, runOnce(b, f))
	}
	b.StopTimer()
	reportPercentiles(b, durs)
}

func BenchmarkPipelineRun_NoLimiter(b *testing.B)   { benchPipeline(b, false) }
func BenchmarkPipelineRun_WithLimiter(b *testing.B) { benchPipeline(b, true) }

func reportPercentiles(b *testing.B, durs []time.Duration) {
	if len(durs) == 0 {
		return
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	b.ReportMetric(float64(durs[len(durs)*50/100].Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(durs[len(durs)*99/100].Nanoseconds()), "p99-ns/op")
}

// TestPipelinePerf_Gate is the regression guard. Runs a fixed
// iteration count outside the bench harness so it executes on every
// `go test ./...`. Threshold is gateSlack × CLAUDE.md target — the
// goal is "catch a 5× regression," not "validate the SLO."
//
// Skipped with -short. The CI matrix should run this without -short;
// laptops can keep -short on for fast iteration.
func TestPipelinePerf_Gate(t *testing.T) {
	if testing.Short() {
		t.Skip("perf gate skipped under -short")
	}

	const iters = 2000
	f := newFixture(true) // worst case: with limiter
	for i := 0; i < 200; i++ {
		_ = runOnce(t, f)
	}
	durs := make([]time.Duration, iters)
	for i := 0; i < iters; i++ {
		durs[i] = runOnce(t, f)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p50 := durs[iters*50/100]
	p99 := durs[iters*99/100]

	p50Limit := time.Duration(float64(p50Target) * gateSlack)
	p99Limit := time.Duration(float64(p99Target) * gateSlack)

	t.Logf("pipeline.Run in-process: p50=%v p99=%v (gate p50<%v p99<%v)",
		p50, p99, p50Limit, p99Limit)
	if p50 > p50Limit {
		t.Errorf("p50 regression: got %v, gate %v (CLAUDE.md target %v)", p50, p50Limit, p50Target)
	}
	if p99 > p99Limit {
		t.Errorf("p99 regression: got %v, gate %v (CLAUDE.md target %v)", p99, p99Limit, p99Target)
	}
}
