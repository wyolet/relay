package pipeline_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
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
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// fakeSnap is a minimal policy.SnapshotReader for tests.
type fakeSnap struct {
	pols map[string]*policy.Policy
	rls  map[string]*ratelimit.RateLimit
}

func (f *fakeSnap) Policy(_ context.Context, id string) (*policy.Policy, bool) {
	p, ok := f.pols[id]
	return p, ok
}
func (f *fakeSnap) RateLimit(_ context.Context, id string) (*ratelimit.RateLimit, bool) {
	r, ok := f.rls[id]
	return r, ok
}

// fakeAdapter answers 200 "ok" unless callFn says otherwise, and counts calls.
type fakeAdapter struct {
	callFn    func(ctx context.Context, baseURL, key string, body []byte, hdr http.Header) (*http.Response, error)
	tokens    pkgusage.Tokens
	retryFn   func(*http.Response) (bool, keypool.FailureKind, time.Duration)
	callCount atomic.Int32
	lastOAuth atomic.Bool
}

func (f *fakeAdapter) Call(ctx context.Context, baseURL string, _ *string, key string, body []byte, hdr http.Header, _ string, _, oauth bool) (*http.Response, error) {
	f.callCount.Add(1)
	f.lastOAuth.Store(oauth)
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
	if snap == nil {
		snap = &fakeSnap{}
	}
	return serviceOver(snap, kv.NewMem())
}

// serviceOver builds a policy service whose key pool and limiter share mem.
func serviceOver(snap policy.SnapshotReader, mem *kv.Mem) *policy.Service {
	return policy.NewService(snap, keypool.New(mem, slog.Default(), nil, nil), pkgratelimit.New(mem, slog.Default(), nil))
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
