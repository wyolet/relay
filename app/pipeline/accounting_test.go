package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// sseAdapter serves a fixed SSE body. ExtractTokens reports tokens only when
// the buffer it is handed contains the usage frame, as vendor extractors do.
type sseAdapter struct {
	body   string
	callFn func(ctx context.Context) error
}

func (a *sseAdapter) Call(ctx context.Context, _ string, _ *string, _ string, _ []byte, _ http.Header, _ string, _, _ bool) (*http.Response, error) {
	if a.callFn != nil {
		if err := a.callFn(ctx); err != nil {
			return nil, err
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(a.body))}, nil
}

func (a *sseAdapter) ExtractTokens(body []byte) pkgusage.Tokens {
	if bytes.Contains(body, []byte(`"usage"`)) {
		return pkgusage.Tokens{"input": 40, "output": 60}
	}
	return nil
}

func (a *sseAdapter) Retryable(*http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

func budgetPolicy(meter ratelimit.Meter, strategy ratelimit.Strategy, amount int64) (*policy.Policy, policy.SnapshotReader) {
	pol := &policy.Policy{Meta: meta.Metadata{ID: "bpol", Name: "budget-policy"}, Spec: policy.Spec{RateLimitID: "brl"}}
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: "brl", Name: "brl"},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: meter, Amount: amount, Window: ratelimit.Window(time.Hour), Strategy: strategy}}},
	}
	return pol, &fakeSnap{pols: map[string]*policy.Policy{"bpol": pol}, rls: map[string]*ratelimit.RateLimit{"brl": rl}}
}

func budgetPipeline(t *testing.T, snap policy.SnapshotReader) (*pipeline.Pipeline, *kv.Mem) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	return &pipeline.Pipeline{Policy: serviceOver(snap, mem), Logger: slog.Default()}, mem
}

func waitBudgetCommit(t *testing.T, mem *kv.Mem) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ents, _ := mem.Range(context.Background(), "limit:{budget-policy}:committed:")
		if len(ents) >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("post-flight commit did not land")
}

const (
	sseContent = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n"
	sseUsage   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":60}}\n\ndata: [DONE]\n\n"
)

// A caller that closes the body before the terminal usage frame is charged
// the same tokens as one that drains it.
func TestEarlyCloseStillChargesTokens(t *testing.T) {
	pol, snap := budgetPolicy(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 1)
	for _, drain := range []bool{true, false} {
		pl, mem := budgetPipeline(t, snap)
		ad := &sseAdapter{body: sseContent + sseUsage}
		mk := func() *pipeline.Request {
			return &pipeline.Request{Body: []byte("{}"), Headers: http.Header{}, Adapter: ad, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}, Stream: true}
		}
		res, err := pl.Run(context.Background(), mk())
		if err != nil {
			t.Fatalf("first run: %v", err)
		}
		if drain {
			_, _ = io.Copy(io.Discard, res.Body)
		} else {
			buf := make([]byte, len(sseContent))
			_, _ = io.ReadFull(res.Body, buf)
		}
		_ = res.Body.Close()
		waitBudgetCommit(t, mem)

		_, err = pl.Run(context.Background(), mk())
		var ex *pkgratelimit.KeyQuotaExhausted
		if !errors.As(err, &ex) {
			t.Fatalf("drained=%v: second request err = %v, want tokens budget exceeded", drain, err)
		}
	}
}

// pipeAdapter streams whatever the test writes into the pipe and, like a real
// HTTP body, fails reads once the upstream call context is cancelled.
type pipeAdapter struct {
	sseAdapter
	pr *io.PipeReader
}

func (a *pipeAdapter) Call(ctx context.Context, _ string, _ *string, _ string, _ []byte, _ http.Header, _ string, _, _ bool) (*http.Response, error) {
	context.AfterFunc(ctx, func() { a.pr.CloseWithError(context.Canceled) })
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: a.pr}, nil
}

// A caller that disconnects after the response started and closes before the
// usage frame is still charged the reported tokens: the upstream outlives
// the caller long enough for post-flight to read the tail.
func TestCallerGoneBeforeUsageFrameStillChargesTokens(t *testing.T) {
	pol, snap := budgetPolicy(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 1)
	pl, mem := budgetPipeline(t, snap)
	pr, pw := io.Pipe()
	ad := &pipeAdapter{pr: pr}

	ctx, cancel := context.WithCancel(context.Background())
	res, err := pl.Run(ctx, &pipeline.Request{Body: []byte("{}"), Headers: http.Header{}, Adapter: ad, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}, Stream: true})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	go func() { _, _ = pw.Write([]byte(sseContent)) }()
	if _, err := io.ReadFull(res.Body, make([]byte, len(sseContent))); err != nil {
		t.Fatalf("read content frame: %v", err)
	}
	cancel()
	_ = res.Body.Close()
	time.Sleep(50 * time.Millisecond)
	_, _ = pw.Write([]byte(sseUsage))
	_ = pw.Close()
	waitBudgetCommit(t, mem)

	_, err = pl.Run(context.Background(), &pipeline.Request{Body: []byte("{}"), Headers: http.Header{}, Adapter: &sseAdapter{body: "{}"}, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}})
	var ex *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &ex) {
		t.Fatalf("second request err = %v, want tokens budget exceeded", err)
	}
}

// An upstream that keeps the response open after the caller closed is cut off
// by the drain bound, and post-flight still commits.
func TestEarlyCloseDrainIsBounded(t *testing.T) {
	t.Parallel()
	pol, snap := budgetPolicy(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 1000)
	pl, mem := budgetPipeline(t, snap)
	pr, pw := io.Pipe()
	defer pw.Close()
	ad := &pipeAdapter{pr: pr}

	res, err := pl.Run(context.Background(), &pipeline.Request{Body: []byte("{}"), Headers: http.Header{}, Adapter: ad, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}, Stream: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	go func() { _, _ = pw.Write([]byte(sseContent)) }()
	if _, err := io.ReadFull(res.Body, make([]byte, len(sseContent))); err != nil {
		t.Fatalf("read content frame: %v", err)
	}
	_ = res.Body.Close()
	waitBudgetCommit(t, mem)
	if _, err := pw.Write([]byte("x")); err == nil {
		t.Fatal("upstream body still open after the drain bound")
	}
}

// cutReader yields its content, then fails as an upstream reset would.
type cutReader struct{ r io.Reader }

func (c *cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// A response that ends before the upstream reported usage charges a floor
// (request size for input, frames seen for output) instead of nothing.
func TestCutResponseChargesTokenFloor(t *testing.T) {
	pol, snap := budgetPolicy(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 50)
	pl, mem := budgetPipeline(t, snap)
	ad := &cutAdapter{sseAdapter{body: sseContent}}
	reqBody := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 400) + `"}]}`)

	res, err := pl.Run(context.Background(), &pipeline.Request{Body: reqBody, Headers: http.Header{}, Adapter: ad, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}, Stream: true})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read err = %v, want the cut", err)
	}
	_ = res.Body.Close()
	waitBudgetCommit(t, mem)

	_, err = pl.Run(context.Background(), &pipeline.Request{Body: reqBody, Headers: http.Header{}, Adapter: &sseAdapter{body: "{}"}, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}})
	var ex *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &ex) {
		t.Fatalf("second request err = %v, want tokens budget exceeded", err)
	}
}

type cutAdapter struct{ sseAdapter }

func (a *cutAdapter) Call(context.Context, string, *string, string, []byte, http.Header, string, bool, bool) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(&cutReader{r: strings.NewReader(a.body)})}, nil
}

// A caller that cancels after the request reached the upstream keeps its
// request charge: the next request under a 1-request budget is rejected.
func TestCancelAfterDispatchKeepsRequestCharge(t *testing.T) {
	pol, snap := budgetPolicy(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 1)
	pl, _ := budgetPipeline(t, snap)

	received := make(chan struct{})
	ad := &sseAdapter{body: "{}", callFn: func(ctx context.Context) error {
		close(received)
		<-ctx.Done()
		return ctx.Err()
	}}
	mk := func(a pipeline.Adapter) *pipeline.Request {
		return &pipeline.Request{Body: []byte("{}"), Headers: http.Header{}, Adapter: a, Policy: pol, Keys: []*hostkey.HostKey{makeKey("bk", "sk")}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := pl.Run(ctx, mk(ad)); done <- err }()
	<-received
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run err = %v, want context.Canceled", err)
	}

	res, err := pl.Run(context.Background(), mk(&sseAdapter{body: "{}"}))
	if res != nil {
		drainResult(t, res)
	}
	var ex *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &ex) {
		t.Fatalf("second request after a dispatched-then-cancelled request: err = %v, want requests budget exceeded", err)
	}
}
