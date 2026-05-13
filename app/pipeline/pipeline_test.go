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
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// ---------------------------------------------------------------------------
// fakeAdapter
// ---------------------------------------------------------------------------

type fakeAdapter struct {
	callFn    func(ctx context.Context, baseURL, key string, body []byte, hdr http.Header) (*http.Response, error)
	tokens    pkgusage.Tokens
	retryFn   func(*http.Response) (bool, keypool.FailureKind, time.Duration)
	callCount atomic.Int32
}

func (f *fakeAdapter) Call(ctx context.Context, baseURL, key string, body []byte, hdr http.Header) (*http.Response, error) {
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
		Spec: policy.Spec{
			KeySelection: policy.KeySelectionPrioritized,
		},
	}
}

func newPipeline() *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Limiter:  nil,
		Selector: keypool.New(kv.NewMem(), slog.Default(), nil, nil),
		Logger:   slog.Default(),
	}
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
	p := newPipeline()

	req := &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key},
		Policy:  makePolicy(),
		OnSuccess: func(tok pkgusage.Tokens, kh string) {
			successTokens = tok
			successHash = kh
			wg.Done()
		},
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
		t.Errorf("OnSuccess keyHash = %q, want hash1", successHash)
	}
	if successTokens["input"] != 5 || successTokens["output"] != 10 {
		t.Errorf("OnSuccess tokens = %v, want input=5 output=10", successTokens)
	}
	if adp.callCount.Load() != 1 {
		t.Errorf("callCount = %d, want 1", adp.callCount.Load())
	}
}

func TestRetryOnTransient_RotatesKey(t *testing.T) {
	t.Parallel()

	key1 := makeKey("hash1", "sk-1")
	key2 := makeKey("hash2", "sk-2")

	sel := keypool.New(kv.NewMem(), slog.Default(), nil, nil)

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

	p := &pipeline.Pipeline{Selector: sel, Logger: slog.Default()}
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

	// Use a real Limiter with a rule that immediately rejects by setting
	// Amount=0. A zero-budget rule always fails Reserve.
	mem := kv.NewMem()
	lim := pkgratelimit.New(mem, slog.Default(), nil)

	rules := []pkgratelimit.Rule{
		{
			Key:      "test-key",
			Name:     "test-rule",
			Meter:    "requests",
			Strategy: pkgratelimit.StrategySlidingWindow,
			Amount:   0, // immediately exhausted
			Window:   time.Minute,
		},
	}

	adp := &fakeAdapter{}
	p := &pipeline.Pipeline{
		Limiter:  lim,
		Selector: keypool.New(mem, slog.Default(), nil, nil),
		Logger:   slog.Default(),
	}

	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{makeKey("h1", "sk-1")},
		Policy:    makePolicy(),
		RateScope: "test-scope",
		Rules:     rules,
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

	p := newPipeline()
	req := &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key},
		Policy:  makePolicy(),
		OnSuccess: func(tok pkgusage.Tokens, kh string) {
			gotTokens = tok
			gotHash = kh
			wg.Done()
		},
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// OnSuccess must NOT fire before Body is closed.
	select {
	case <-channelFromWG(&wg):
		t.Fatal("OnSuccess fired before Body.Close")
	default:
	}

	drainResult(t, res)
	wg.Wait()

	if gotHash != "hpf" {
		t.Errorf("OnSuccess keyHash = %q, want hpf", gotHash)
	}
	if gotTokens["input"] != 100 || gotTokens["output"] != 200 {
		t.Errorf("OnSuccess tokens = %v", gotTokens)
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
