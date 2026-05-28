package pipeline_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// fakeSnap is a minimal policy.SnapshotReader for tests.
type fakeSnap struct {
	pols map[string]*policy.Policy
	rls  map[string]*ratelimit.RateLimit
}

func (f *fakeSnap) Policy(id string) (*policy.Policy, bool) {
	p, ok := f.pols[id]
	return p, ok
}
func (f *fakeSnap) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	r, ok := f.rls[id]
	return r, ok
}

// ---------------------------------------------------------------------------
// fakeAdapter
// ---------------------------------------------------------------------------

type fakeAdapter struct {
	callFn    func(ctx context.Context, baseURL, key string, body []byte, hdr http.Header) (*http.Response, error)
	tokens    pkgusage.Tokens
	retryFn   func(*http.Response) (bool, keypool.FailureKind, time.Duration)
	callCount atomic.Int32
}

func (f *fakeAdapter) Call(ctx context.Context, baseURL, key string, body []byte, hdr http.Header, _ string, _ bool) (*http.Response, error) {
	f.callCount.Add(1)
	if f.callFn != nil {
		return f.callFn(ctx, baseURL, key, body, hdr)
	}
	return okResp("ok"), nil
}

func (f *fakeAdapter) ExtractTokens(_ []byte) pkgusage.Tokens {
	if f.tokens != nil {
		return f.tokens
	}
	return pkgusage.Tokens{"input": 10, "output": 20}
}

func (f *fakeAdapter) Retryable(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
	if f.retryFn != nil {
		return f.retryFn(resp)
	}
	return false, 0, 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func okResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

func errResp(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("error")),
		Header:     http.Header{},
	}
}

func makeKey(hash, resolved string) *hostkey.HostKey {
	return &hostkey.HostKey{
		Resolved: resolved,
		KeyHash:  hash,
	}
}

func makePolicy() *policy.Policy {
	return &policy.Policy{
		Meta: meta.Metadata{Name: "test-policy"},
		Spec: policy.Spec{KeySelection: policy.KeySelectionPrioritized},
	}
}

func newService(snap policy.SnapshotReader) *policy.Service {
	mem := kv.NewMem()
	sel := keypool.New(mem, slog.Default(), nil, nil)
	lim := pkgratelimit.New(mem, slog.Default(), nil)
	if snap == nil {
		snap = &fakeSnap{}
	}
	return policy.NewService(snap, sel, lim)
}

func newPipeline() *pipeline.Pipeline {
	return &pipeline.Pipeline{Policy: newService(nil), Logger: slog.Default()}
}

func drainResult(t *testing.T, res *pipeline.Result) {
	t.Helper()
	_, _ = io.Copy(io.Discard, res.Body)
	if err := res.Body.Close(); err != nil {
		t.Errorf("result Body.Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHappyPath_SinglePass(t *testing.T) {
	t.Parallel()

	var successTokens pkgusage.Tokens
	var successHash string
	var wg sync.WaitGroup
	wg.Add(1)

	key := makeKey("hash1", "sk-abc")
	adp := &fakeAdapter{tokens: pkgusage.Tokens{"input": 5, "output": 10}}

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		successTokens = adp.ExtractTokens(ev.ResponseBody)
		successHash = lc.HostKeyID
		wg.Done()
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg

	req := &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{key},
		Policy:    makePolicy(),
		Lifecycle: lifecycle.NewContext("req-happy", "pipeline", time.Now()),
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.KeyHash != "hash1" {
		t.Errorf("KeyHash = %q, want hash1", res.KeyHash)
	}
	if res.Status != 200 {
		t.Errorf("Status = %d, want 200", res.Status)
	}

	drainResult(t, res)
	wg.Wait()

	if successHash != "hash1" {
		t.Errorf("post-flight keyHash = %q, want hash1", successHash)
	}
	if successTokens["input"] != 5 || successTokens["output"] != 10 {
		t.Errorf("post-flight tokens = %v, want input=5 output=10", successTokens)
	}
	if adp.callCount.Load() != 1 {
		t.Errorf("callCount = %d, want 1", adp.callCount.Load())
	}
}

func TestPostFlight_TimingMarks(t *testing.T) {
	t.Parallel()

	var got lifecycle.Timing
	var gotAttempts int
	var wg sync.WaitGroup
	wg.Add(1)

	key := makeKey("hash1", "sk-abc")
	adp := &fakeAdapter{tokens: pkgusage.Tokens{"input": 1}}

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		got = lc.Timing
		gotAttempts = lc.Attempts
		wg.Done()
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg

	req := &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{key},
		Policy:    makePolicy(),
		Stream:    true,
		Lifecycle: lifecycle.NewContext("req-timing", "pipeline", time.Now()),
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)
	wg.Wait()

	// Every mark anchored to Start, monotonic non-decreasing, all set.
	if got.Upstream.Start <= 0 {
		t.Errorf("Upstream.Start not stamped: %v", got.Upstream.Start)
	}
	if got.Upstream.ResponseStart < got.Upstream.Start {
		t.Errorf("ResponseStart %v < Start %v", got.Upstream.ResponseStart, got.Upstream.Start)
	}
	if got.Upstream.ResponseEnd < got.Upstream.ResponseStart {
		t.Errorf("ResponseEnd %v < ResponseStart %v", got.Upstream.ResponseEnd, got.Upstream.ResponseStart)
	}
	if got.End < got.Upstream.ResponseEnd {
		t.Errorf("End %v < ResponseEnd %v", got.End, got.Upstream.ResponseEnd)
	}
	if gotAttempts != 1 {
		t.Errorf("Attempts = %d, want 1 (single key, first-try success)", gotAttempts)
	}
}

func TestRun_NoKeysEmitsFailureEvent(t *testing.T) {
	t.Parallel()

	var gotKind string
	var gotStatus int
	var wg sync.WaitGroup
	wg.Add(1)

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		gotKind, gotStatus = ev.ErrorKind, ev.Status
		wg.Done()
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg

	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:   &fakeAdapter{},
		Keys:      nil, // → ErrNoKeys
		Policy:    makePolicy(),
		Lifecycle: lifecycle.NewContext("req-nokeys", "pipeline", time.Now()),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	wg.Wait()
	if gotKind != "no_keys" {
		t.Errorf("ErrorKind = %q, want no_keys", gotKind)
	}
	if gotStatus != 0 {
		t.Errorf("Status = %d, want 0 (upstream never reached)", gotStatus)
	}
}

func TestRun_UpstreamFailureEmitsEvent(t *testing.T) {
	t.Parallel()

	var gotKind string
	var gotStatus int
	var wg sync.WaitGroup
	wg.Add(1)

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		gotKind, gotStatus = ev.ErrorKind, ev.Status
		wg.Done()
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
			return errResp(503), nil
		},
		retryFn: func(*http.Response) (bool, keypool.FailureKind, time.Duration) {
			return true, keypool.FailureServerError, 0
		},
	}
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{makeKey("h1", "sk-1")},
		Policy:    makePolicy(),
		Lifecycle: lifecycle.NewContext("req-upfail", "pipeline", time.Now()),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	wg.Wait()
	if gotKind != "upstream_error" {
		t.Errorf("ErrorKind = %q, want upstream_error", gotKind)
	}
	if gotStatus != 503 {
		t.Errorf("Status = %d, want 503 (surfaced upstream status)", gotStatus)
	}
}

func TestRetryOnTransient_RotatesKey(t *testing.T) {
	t.Parallel()

	key1 := makeKey("hash1", "sk-1")
	key2 := makeKey("hash2", "sk-2")

	netErr := errors.New("network error")
	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
			if key == "sk-1" {
				return nil, netErr
			}
			return okResp("ok"), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			return false, 0, 0
		},
	}

	p := newPipeline()
	req := &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.KeyHash != "hash2" {
		t.Errorf("KeyHash = %q, want hash2 (second key)", res.KeyHash)
	}
	drainResult(t, res)

	if adp.callCount.Load() != 2 {
		t.Errorf("callCount = %d, want 2", adp.callCount.Load())
	}
}

func TestRetryOn429_Long_RecordsRateLimit(t *testing.T) {
	t.Parallel()

	key1 := makeKey("hash1", "sk-1")
	key2 := makeKey("hash2", "sk-2")

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
			if key == "sk-1" {
				return errResp(429), nil
			}
			return okResp("ok"), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			if resp.StatusCode == 429 {
				return true, keypool.FailureRateLimitLong, 30 * time.Second
			}
			return false, 0, 0
		},
	}

	p := newPipeline()
	req := &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.KeyHash != "hash2" {
		t.Errorf("KeyHash = %q, want hash2", res.KeyHash)
	}
	drainResult(t, res)
	// key1 should be circuit-open; verify by checking it's excluded on subsequent picks
	if adp.callCount.Load() != 2 {
		t.Errorf("callCount = %d, want 2", adp.callCount.Load())
	}
}

func TestRetryOn5xx_ServerError(t *testing.T) {
	t.Parallel()

	key1 := makeKey("h1", "sk-1")
	key2 := makeKey("h2", "sk-2")

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
			if key == "sk-1" {
				return errResp(503), nil
			}
			return okResp("ok"), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			if resp.StatusCode >= 500 {
				return true, keypool.FailureServerError, 0
			}
			return false, 0, 0
		},
	}

	p := newPipeline()
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.KeyHash != "h2" {
		t.Errorf("KeyHash = %q, want h2", res.KeyHash)
	}
	drainResult(t, res)
	if adp.callCount.Load() != 2 {
		t.Errorf("callCount = %d, want 2", adp.callCount.Load())
	}
}

func TestRetryOn401_AuthFail(t *testing.T) {
	t.Parallel()

	key1 := makeKey("h1", "sk-bad")
	key2 := makeKey("h2", "sk-good")

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
			if key == "sk-bad" {
				return errResp(401), nil
			}
			return okResp("ok"), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			if resp.StatusCode == 401 {
				return true, keypool.FailureAuth, 0
			}
			return false, 0, 0
		},
	}

	p := newPipeline()
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.KeyHash != "h2" {
		t.Errorf("KeyHash = %q, want h2", res.KeyHash)
	}
	drainResult(t, res)
}

func TestNonRetryable_4xx_PassesThrough(t *testing.T) {
	t.Parallel()

	key1 := makeKey("h1", "sk-1")
	key2 := makeKey("h2", "sk-2")

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
			return errResp(400), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			return false, 0, 0 // not retryable
		},
	}

	p := newPipeline()
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != 400 {
		t.Errorf("Status = %d, want 400", res.Status)
	}
	drainResult(t, res)

	if adp.callCount.Load() != 1 {
		t.Errorf("callCount = %d, want 1 (no rotation)", adp.callCount.Load())
	}
}

func TestAllKeysExhausted_Returns503Sentinel(t *testing.T) {
	t.Parallel()

	key1 := makeKey("h1", "sk-1")
	key2 := makeKey("h2", "sk-2")

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
			return errResp(500), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			return true, keypool.FailureServerError, 0
		},
	}

	p := newPipeline()
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:     adp,
		Keys:        []*hostkey.HostKey{key1, key2},
		Policy:      makePolicy(),
		MaxAttempts: 2,
	})
	if res != nil {
		drainResult(t, res)
		t.Fatal("expected nil result")
	}
	if !errors.Is(err, pipeline.ErrAllKeysExhausted) {
		t.Errorf("err = %v, want ErrAllKeysExhausted", err)
	}
	if adp.callCount.Load() != 2 {
		t.Errorf("callCount = %d, want 2", adp.callCount.Load())
	}
}

func TestNoKeys_Returns_ErrNoKeys(t *testing.T) {
	t.Parallel()

	p := newPipeline()
	adp := &fakeAdapter{}
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    nil,
		Policy:  makePolicy(),
	})
	if !errors.Is(err, pipeline.ErrNoKeys) {
		t.Errorf("err = %v, want ErrNoKeys", err)
	}
	if adp.callCount.Load() != 0 {
		t.Errorf("callCount = %d, want 0", adp.callCount.Load())
	}
}

func TestAdapterMissing_ReturnsErr(t *testing.T) {
	t.Parallel()

	p := newPipeline()
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: nil,
		Keys:    []*hostkey.HostKey{makeKey("h1", "sk-1")},
		Policy:  makePolicy(),
	})
	if !errors.Is(err, pipeline.ErrAdapterMissing) {
		t.Errorf("err = %v, want ErrAdapterMissing", err)
	}
}

func TestMaxAttempts_Capped(t *testing.T) {
	t.Parallel()

	keys := []*hostkey.HostKey{
		makeKey("h1", "sk-1"),
		makeKey("h2", "sk-2"),
		makeKey("h3", "sk-3"),
	}

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
			return errResp(500), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			return true, keypool.FailureServerError, 0
		},
	}

	p := newPipeline()
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:     adp,
		Keys:        keys,
		Policy:      makePolicy(),
		MaxAttempts: 1,
	})
	if !errors.Is(err, pipeline.ErrAllKeysExhausted) {
		t.Errorf("err = %v, want ErrAllKeysExhausted", err)
	}
	if adp.callCount.Load() != 1 {
		t.Errorf("callCount = %d, want 1 (MaxAttempts=1)", adp.callCount.Load())
	}
}

func TestReserveFails_BubblesUp(t *testing.T) {
	t.Parallel()

	// Policy with an RL whose single rule has Amount=0 → always exhausted.
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: "rl-zero", Name: "zero"},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 0,
			Window: time.Minute, Strategy: ratelimit.StrategySlidingWindow,
		}}},
	}
	pol := makePolicy()
	pol.Spec.RateLimitID = rl.Meta.ID

	svc := newService(&fakeSnap{rls: map[string]*ratelimit.RateLimit{rl.Meta.ID: rl}})
	p := &pipeline.Pipeline{Policy: svc, Logger: slog.Default()}

	adp := &fakeAdapter{}
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{makeKey("h1", "sk-1")},
		Policy:  pol,
	})

	if err == nil {
		t.Fatal("expected error from Reserve, got nil")
	}
	if !errors.Is(err, pkgratelimit.ErrExceeded) {
		t.Errorf("err = %v, want wrapping ErrExceeded", err)
	}
	if adp.callCount.Load() != 0 {
		t.Errorf("callCount = %d, want 0 (Reserve rejected before Call)", adp.callCount.Load())
	}
}

func TestPostFlight_CommitsOnBodyClose(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	wg.Add(1)

	var gotTokens pkgusage.Tokens
	var gotHash string

	key := makeKey("hpf", "sk-pf")
	adp := &fakeAdapter{
		tokens: pkgusage.Tokens{"input": 100, "output": 200},
	}

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		gotTokens = adp.ExtractTokens(ev.ResponseBody)
		gotHash = lc.HostKeyID
		wg.Done()
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg
	req := &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{key},
		Policy:    makePolicy(),
		Lifecycle: lifecycle.NewContext("req-pf", "pipeline", time.Now()),
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Post-flight must NOT fire before Body is closed.
	select {
	case <-channelFromWG(&wg):
		t.Fatal("post-flight fired before Body.Close")
	default:
	}

	drainResult(t, res)
	wg.Wait()

	if gotHash != "hpf" {
		t.Errorf("post-flight keyHash = %q, want hpf", gotHash)
	}
	if gotTokens["input"] != 100 || gotTokens["output"] != 200 {
		t.Errorf("post-flight tokens = %v", gotTokens)
	}
}

// channelFromWG returns a channel that is closed once the WaitGroup reaches zero.
func channelFromWG(wg *sync.WaitGroup) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch
}

func TestMaxAttempts_DefaultsToThree(t *testing.T) {
	t.Parallel()

	// 4 keys; first 3 always fail. Default MaxAttempts=3 should cap at 3.
	keys := []*hostkey.HostKey{
		makeKey("h1", "sk-1"),
		makeKey("h2", "sk-2"),
		makeKey("h3", "sk-3"),
		makeKey("h4", "sk-4"), // must never be reached
	}

	adp := &fakeAdapter{
		callFn: func(_ context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
			return errResp(500), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			return true, keypool.FailureServerError, 0
		},
	}

	p := newPipeline()
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:     adp,
		Keys:        keys,
		Policy:      makePolicy(),
		MaxAttempts: 0, // must default to 3
	})
	if !errors.Is(err, pipeline.ErrAllKeysExhausted) {
		t.Errorf("err = %v, want ErrAllKeysExhausted", err)
	}
	if adp.callCount.Load() != 3 {
		t.Errorf("callCount = %d, want 3 (defaultMaxAttempts)", adp.callCount.Load())
	}
}
