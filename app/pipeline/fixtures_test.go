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

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

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
		Meta:     meta.Metadata{ID: "key-" + hash},
		Resolved: resolved,
		KeyHash:  hash,
	}
}

// testHost is the host a hop built without one calls.
var testHost = &host.Host{Meta: meta.Metadata{ID: "host-test", Name: "test-host"}}

// hopOf is one prioritized pool at testHost holding keys, with nothing to count.
func hopOf(keys []*hostkey.HostKey) []plan.RouteHop { return []plan.RouteHop{hopAt(testHost, keys)} }

// hopAt is one prioritized pool at h holding keys, with nothing to count.
func hopAt(h *host.Host, keys []*hostkey.HostKey) plan.RouteHop {
	pp := &plan.PoolPlan{Pool: &pool.Pool{Meta: meta.Metadata{ID: "pool-" + h.Meta.ID}}, Host: h, Selection: keypool.KeySelectionPrioritized}
	hop := plan.RouteHop{Pool: pp, Host: h}
	for _, k := range keys {
		s := &plan.KeySlot{Credential: k, Host: h}
		pp.Slots = append(pp.Slots, s)
		hop.Slots = append(hop.Slots, s)
		hop.Credentials = append(hop.Credentials, k)
		hop.Counters = append(hop.Counters, nil)
	}
	return hop
}

// withAccount puts every slot of hop into one provider account with a cooldown.
func withAccount(hop plan.RouteHop, id string) plan.RouteHop {
	acc := &plan.AccountPlan{Account: &account.Account{Meta: meta.Metadata{ID: id}}, Cooldown: keypool.NewCooldownCell()}
	for _, s := range hop.Slots {
		s.Account = acc
	}
	return hop
}

// counted gives every slot of hop the caller counters, then its own upstream ones.
func counted(hop plan.RouteHop, caller []limits.Counter, upstream func(i int) []limits.Counter) plan.RouteHop {
	hop.CallerN = len(caller)
	for i := range hop.Slots {
		cs := append([]limits.Counter(nil), caller...)
		if upstream != nil {
			cs = append(cs, upstream(i)...)
		}
		hop.Counters[i] = cs
	}
	return hop
}

var testRateLimit = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-test"}}

// callerRule is a caller counter of amount requests per minute.
func callerRule(amount int64) limits.Counter {
	r := ratelimit.Rule{Meter: ratelimit.MeterRequests, Strategy: ratelimit.StrategyFixedWindow, Amount: amount, Window: ratelimit.Window(time.Minute)}
	return limits.CallerRule(limits.RungProject, testRateLimit, r, "pol-caller", "project:p", "team:t")
}

// accountRule is an account counter of amount requests per minute on account id.
func accountRule(id string, amount int64) limits.Counter {
	r := ratelimit.Rule{Meter: ratelimit.MeterRequests, Strategy: ratelimit.StrategyFixedWindow, Amount: amount, Window: ratelimit.Window(time.Minute)}
	return limits.UpstreamRule(limits.RungAccount, testRateLimit, r, "pol-tier", "account:"+id, "", "account:"+id)
}

// limiterOver is the Lua limiter over s.
func limiterOver(s kv.Store) limits.Limiter {
	if m, ok := s.(*kv.Mem); ok {
		pkgratelimit.RegisterScripts(m)
	}
	return lua.New(pkgratelimit.New(s, slog.Default(), nil))
}

// pipelineOver builds a pipeline whose limiter and key pool share mem.
func pipelineOver(mem *kv.Mem) *pipeline.Pipeline {
	return &pipeline.Pipeline{Limiter: limiterOver(mem), Selector: keypool.New(mem, slog.Default(), nil, nil), Logger: slog.Default()}
}

func newPipeline() *pipeline.Pipeline { return pipelineOver(kv.NewMem()) }

func drainResult(t *testing.T, res *pipeline.Result) {
	t.Helper()
	_, _ = io.Copy(io.Discard, res.Body)
	if err := res.Body.Close(); err != nil {
		t.Errorf("result Body.Close: %v", err)
	}
}
