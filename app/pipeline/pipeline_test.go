package pipeline_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/meta"
)

var errDial = &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}

// keyedAdapter answers per credential value: a call error, or a status with the body "error" (200 "ok" for an unlisted value). It records the values called, in order, and classifies like the spec adapter: 401, 403, 429 and 5xx fail over; a 429 is long when retryAfter passes five seconds.
type keyedAdapter struct {
	fakeAdapter
	status     map[string]int
	errs       map[string]error
	retryAfter time.Duration
	mu         sync.Mutex
	calls      []string
}

func newKeyedAdapter() *keyedAdapter {
	a := &keyedAdapter{status: map[string]int{}, errs: map[string]error{}}
	a.callFn = func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
		a.mu.Lock()
		a.calls = append(a.calls, key)
		a.mu.Unlock()
		if err := a.errs[key]; err != nil {
			return nil, err
		}
		if s := a.status[key]; s != 0 {
			return errResp(s), nil
		}
		return okResp("ok"), nil
	}
	a.retryFn = func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
		switch s := resp.StatusCode; {
		case s == 401:
			return true, keypool.FailureAuth, 0
		case s == 403:
			return true, keypool.FailureForbidden, 0
		case s == 429 && a.retryAfter > 5*time.Second:
			return true, keypool.FailureRateLimitLong, a.retryAfter
		case s == 429:
			return true, keypool.FailureRateLimitShort, a.retryAfter
		case s >= 500:
			return true, keypool.FailureServerError, 0
		}
		return false, 0, 0
	}
	return a
}

// breaker is the part of a key's stored record a failed attempt decides.
type breaker struct {
	State    keypool.CircuitState
	Reason   keypool.CooldownReason
	Failures int
	Step     int
}

// wantBreaker waits for keyHash's detached failure write to leave want; nil means the key keeps no record.
func wantBreaker(t *testing.T, sel *keypool.Selector, keyHash string, want *breaker) {
	t.Helper()
	var got *breaker
	deadline := time.Now().Add(2 * time.Second)
	for {
		got = nil
		if rec, found := sel.ReadCircuit(context.Background(), keyHash); found {
			got = &breaker{rec.State, rec.Reason, rec.Failures, rec.BackoffStep}
		}
		if cmp.Equal(want, got) || want == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("breaker of %s (-want +got):\n%s", keyHash, d)
	}
}

// Failover happens before the first byte, over the hop's credentials in order; what the failed credential's breaker keeps depends on how it failed. A dial failure is the host's: the same credential re-dials, no other credential of the host is tried, and no breaker moves.
func TestRun_FailoverPerFailureKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		status     int
		err        error
		retryAfter time.Duration
		wantCalls  []string
		wantStatus int // 0: Run fails
		wantKey    string
		wantH1     *breaker
	}{
		{name: "first credential serves", wantCalls: []string{"sk-1"}, wantStatus: 200, wantKey: "h1"},
		{name: "400 passes through", status: 400, wantCalls: []string{"sk-1"}, wantStatus: 400, wantKey: "h1"},
		{name: "network error is counted", err: errors.New("connection reset"), wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2",
			wantH1: &breaker{Failures: 1}},
		{name: "5xx is counted", status: 503, wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2",
			wantH1: &breaker{Failures: 1}},
		{name: "401 opens the key", status: 401, wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2",
			wantH1: &breaker{State: keypool.CircuitOpen, Reason: keypool.ReasonUpstreamAuthFailed, Step: 1}},
		{name: "403 opens the key", status: 403, wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2",
			wantH1: &breaker{State: keypool.CircuitOpen, Reason: keypool.ReasonUpstreamForbidden, Step: 1}},
		{name: "429 with a long wait opens the key", status: 429, retryAfter: 30 * time.Second, wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2",
			wantH1: &breaker{State: keypool.CircuitOpen, Reason: keypool.ReasonUpstreamRateLimited}},
		{name: "429 with a short wait leaves the key closed", status: 429, wantCalls: []string{"sk-1", "sk-2"}, wantStatus: 200, wantKey: "h2"},
		{name: "dial failure re-dials the host", err: errDial, wantCalls: []string{"sk-1", "sk-1", "sk-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := kv.NewMem()
			t.Cleanup(func() { _ = mem.Close() })
			p := pipelineOver(mem)
			adp := newKeyedAdapter()
			adp.status["sk-1"], adp.errs["sk-1"], adp.retryAfter = tc.status, tc.err, tc.retryAfter
			lc := lifecycle.NewContext("req", "pipeline", time.Now())
			res, err := p.Run(context.Background(), &pipeline.Request{
				Adapter: adp, Lifecycle: lc,
				Hops: hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2")}),
			})
			if d := cmp.Diff(tc.wantCalls, adp.calls); d != "" {
				t.Errorf("credentials called (-want +got):\n%s", d)
			}
			if lc.Attempts != max(len(tc.wantCalls), 1) && tc.err != errDial || lc.HostID != testHost.Meta.ID {
				t.Errorf("lifecycle attempts = %d at host %q, want %d at %q", lc.Attempts, lc.HostID, len(tc.wantCalls), testHost.Meta.ID)
			}
			if tc.wantStatus == 0 {
				var unreachable *pipeline.UpstreamUnreachableError
				if res != nil || !errors.As(err, &unreachable) {
					t.Fatalf("Run = %v, %v; want *UpstreamUnreachableError", res, err)
				}
				want := pipeline.UpstreamUnreachableError{Host: "test-host", Attempts: 3, Cause: errDial, NoneConnected: true}
				if *unreachable != want || lc.Attempts != 1 || lc.HostKeyID != "h1" {
					t.Errorf("error = %+v after %d attempt(s) on %s, want %+v after 1 on h1", *unreachable, lc.Attempts, lc.HostKeyID, want)
				}
			} else {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				body, _ := io.ReadAll(res.Body)
				wantBody := map[bool]string{true: "ok", false: "error"}[tc.wantStatus == 200]
				if res.Status != tc.wantStatus || res.KeyHash != tc.wantKey || string(body) != wantBody || lc.HostKeyID != tc.wantKey {
					t.Errorf("served %d %q by %s (lifecycle %s), want %d %q by %s", res.Status, body, res.KeyHash, lc.HostKeyID, tc.wantStatus, wantBody, tc.wantKey)
				}
				if err := res.Body.Close(); err != nil {
					t.Errorf("Body.Close: %v", err)
				}
			}
			wantBreaker(t, p.Selector, "h1", tc.wantH1)
			wantBreaker(t, p.Selector, "h2", nil)
		})
	}
}

// Once an upstream's response is accepted the request is that credential's: a body that breaks mid-stream reaches the caller as it is, no other credential is called, and the key keeps no failure.
func TestRun_NoFailoverOnceTheResponseStarted(t *testing.T) {
	t.Parallel()
	errCut := errors.New("stream cut")
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	health := make(hostHealthLog, 2)
	p := pipelineOver(mem)
	p.HostHealth = health
	adp := newKeyedAdapter()
	inner := adp.callFn
	adp.callFn = func(ctx context.Context, baseURL, key string, body []byte, hdr http.Header) (*http.Response, error) {
		resp, err := inner(ctx, baseURL, key, body, hdr)
		resp.Body = io.NopCloser(io.MultiReader(strings.NewReader("data: partial"), iotest.ErrReader(errCut)))
		return resp, err
	}
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp, Stream: true,
		Hops: hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2")}),
	})
	if err != nil || res.Status != 200 || res.KeyHash != "h1" {
		t.Fatalf("Run = %+v, %v; want 200 served by h1", res, err)
	}
	body, readErr := io.ReadAll(res.Body)
	if string(body) != "data: partial" || !errors.Is(readErr, errCut) {
		t.Errorf("body = %q, %v; want the partial bytes and the upstream's read error", body, readErr)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Body.Close: %v", err)
	}
	if got := <-health; got != "reachable:host-test" {
		t.Errorf("host health = %q, want reachable:host-test", got)
	}
	if d := cmp.Diff([]string{"sk-1"}, adp.calls); d != "" {
		t.Errorf("credentials called (-want +got):\n%s", d)
	}
	wantBreaker(t, p.Selector, "h1", nil)
}

// Without a selector there are no breakers to read: credentials are taken in the hop's order, each failed one left out of the next pick.
func TestRun_WithoutSelectorTakesCredentialsInOrder(t *testing.T) {
	t.Parallel()
	adp := newKeyedAdapter()
	adp.status["sk-1"], adp.status["sk-2"] = 503, 401
	p := &pipeline.Pipeline{Limiter: limiterOver(kv.NewMem()), Logger: slog.Default()}
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Hops:    hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2"), makeKey("h3", "sk-3")}),
	})
	if err != nil || res.Status != 200 || res.KeyHash != "h3" {
		t.Fatalf("Run = %+v, %v; want 200 served by h3", res, err)
	}
	drainResult(t, res)
	if d := cmp.Diff([]string{"sk-1", "sk-2", "sk-3"}, adp.calls); d != "" {
		t.Errorf("credentials called (-want +got):\n%s", d)
	}
}

// Every attempt failing retryably ends in ErrAllKeysExhausted carrying the last upstream response whole, after min(credentials, MaxAttempts) calls in order; MaxAttempts 0 means three.
func TestRun_ExhaustsCredentialsUpToMaxAttempts(t *testing.T) {
	t.Parallel()
	bigBody := `{"type":"error","error":{"message":"` + strings.Repeat("x", 2000) + `"}}`
	for _, tc := range []struct {
		name        string
		keys        []string
		maxAttempts int
		wantCalls   []string
	}{
		{"every credential tried", []string{"sk-1", "sk-2"}, 2, []string{"sk-1", "sk-2"}},
		{"capped below the credential count", []string{"sk-1", "sk-2", "sk-3"}, 1, []string{"sk-1"}},
		{"unset defaults to three", []string{"sk-1", "sk-2", "sk-3", "sk-4"}, 0, []string{"sk-1", "sk-2", "sk-3"}},
		{"negative defaults to three", []string{"sk-1", "sk-2", "sk-3", "sk-4"}, -1, []string{"sk-1", "sk-2", "sk-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var keys []*hostkey.HostKey
			for _, k := range tc.keys {
				keys = append(keys, makeKey("h-"+k, k))
			}
			adp := newKeyedAdapter()
			adp.callFn = func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
				adp.calls = append(adp.calls, key)
				return &http.Response{
					StatusCode: 429,
					Header:     http.Header{"Retry-After": {"7"}, "X-Served-By": {key}},
					Body:       io.NopCloser(strings.NewReader(bigBody)),
				}, nil
			}
			res, err := newPipeline().Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: hopOf(keys), MaxAttempts: tc.maxAttempts})
			if d := cmp.Diff(tc.wantCalls, adp.calls); d != "" {
				t.Errorf("credentials called (-want +got):\n%s", d)
			}
			var upstream *pipeline.UpstreamFailureError
			if res != nil || !errors.Is(err, pipeline.ErrAllKeysExhausted) || !errors.As(err, &upstream) {
				t.Fatalf("Run = %v, %v; want ErrAllKeysExhausted in an *UpstreamFailureError", res, err)
			}
			last := tc.wantCalls[len(tc.wantCalls)-1]
			wantHeader := http.Header{"Retry-After": {"7"}, "X-Served-By": {last}}
			if upstream.Status != 429 || string(upstream.Body) != bigBody || !cmp.Equal(wantHeader, upstream.Header) {
				t.Errorf("last response = %d %v with %d body bytes, want 429 %v with %d", upstream.Status, upstream.Header, len(upstream.Body), wantHeader, len(bigBody))
			}
			// Logs get an excerpt, never the body.
			if want := "upstream returned 429: " + bigBody[:512]; err.Error() != want {
				t.Errorf("error string = %q, want the 512-byte excerpt", err.Error())
			}
		})
	}
}

// failureEvent is what a request that never produced a response reports to post-flight observers.
type failureEvent struct {
	Kind    string
	Status  int
	Refusal string
	Message string
}

// A request relay turns away before any upstream call fails with its sentinel and reports one failure event.
func TestRun_RefusesBeforeAnyCall(t *testing.T) {
	t.Parallel()
	twoKeys := hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2")})
	for _, tc := range []struct {
		name      string
		hops      []plan.RouteHop
		noAdapter bool
		noLimiter bool
		wantIs    error // nil: a *CallerLimitedError
		want      failureEvent
	}{
		{name: "no hops", wantIs: pipeline.ErrNoKeys,
			want: failureEvent{"no_keys", 503, "host_not_reachable", "no host keys"}},
		{name: "hops without credentials", hops: hopOf(nil), wantIs: pipeline.ErrNoKeys,
			want: failureEvent{"no_keys", 503, "host_not_reachable", "no host keys"}},
		{name: "no adapter", hops: twoKeys, noAdapter: true, wantIs: pipeline.ErrAdapterMissing,
			want: failureEvent{"adapter_missing", 0, "", "pipeline: adapter is nil"}},
		{name: "no limiter", hops: twoKeys, noLimiter: true, wantIs: pipeline.ErrLimiterMissing,
			want: failureEvent{"limiter_missing", 0, "", "pipeline: limiter is nil"}},
		{name: "caller counter refuses", hops: []plan.RouteHop{counted(twoKeys[0], []limits.Counter{callerRule(0)}, nil)},
			want: failureEvent{"rate_limited", 429, "no_capacity", "the rate limit of the project policy pol-caller is exhausted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := make(chan failureEvent, 2)
			reg := lifecycle.New()
			reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
				// The wait a fixed window answers depends on where in the window the test runs.
				msg, _, _ := strings.Cut(ev.ErrorMessage, "; try again in ")
				events <- failureEvent{ev.ErrorKind, ev.Status, ev.Refusal, msg}
				return nil, nil
			}})
			p := newPipeline()
			p.Lifecycle = reg
			if tc.noLimiter {
				p.Limiter = nil
			}
			adp := newKeyedAdapter()
			req := &pipeline.Request{Adapter: adp, Hops: tc.hops, Lifecycle: lifecycle.NewContext("req", "pipeline", time.Now())}
			if tc.noAdapter {
				req.Adapter = nil
			}
			res, err := p.Run(context.Background(), req)
			var limited *pipeline.CallerLimitedError
			if res != nil || (tc.wantIs != nil && !errors.Is(err, tc.wantIs)) || (tc.wantIs == nil && !errors.As(err, &limited)) || len(adp.calls) != 0 {
				t.Fatalf("Run = %v, %v after calls %v; want %v and no call", res, err, adp.calls, tc.wantIs)
			}
			select {
			case got := <-events:
				if got != tc.want {
					t.Errorf("failure event = %+v, want %+v", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no failure event")
			}
		})
	}
}

func TestRun_CallerRefusalNamesTheRungAndPolicy(t *testing.T) {
	t.Parallel()
	hop := counted(hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1")})[0], []limits.Counter{callerRule(0)}, nil)
	_, err := newPipeline().Run(context.Background(), &pipeline.Request{Adapter: &fakeAdapter{}, Hops: []plan.RouteHop{hop}})
	var refused *pipeline.CallerLimitedError
	if !errors.As(err, &refused) || refused.Refused.Counter.Rung != limits.RungProject || refused.Refused.Counter.PolicyID != "pol-caller" {
		t.Fatalf("err = %v, want a caller refusal naming the project rung and its policy", err)
	}
	if got := pipeline.RefusalOf(err); got != plan.RefusalNoCapacity {
		t.Errorf("RefusalOf = %q, want %q", got, plan.RefusalNoCapacity)
	}
}

func TestAnswerOf(t *testing.T) {
	t.Parallel()
	unreachable := func(noneConnected bool) error {
		return &pipeline.UpstreamUnreachableError{Host: "h", Attempts: 3, Cause: errDial, NoneConnected: noneConnected}
	}
	for _, tc := range []struct {
		name string
		err  error
		want plan.Answer
		ok   bool
	}{
		{"every credential over an upstream limit", &pipeline.UpstreamLimitedError{RetryAfter: 1500 * time.Millisecond}, plan.Answer{
			Status: 429, Type: "rate_limit_error", Code: "rate_limit_exceeded", Kind: plan.RefusalNoCapacity, RetryAfter: 2,
			Message: "every credential for this model is over its upstream rate limit; try again in 2s",
		}, true},
		{"every key out of rotation", keypool.ErrNoHealthyKeys, plan.Answer{
			Status: 503, Type: "server_error", Code: "no_healthy_keys", Kind: plan.RefusalNoCapacity,
			Message: "every host key for this model is out of rotation after failing; retry later",
		}, true},
		{"no host connected", unreachable(true), plan.Answer{
			Status: 502, Type: "server_error", Code: "upstream_unreachable", Kind: plan.RefusalHostNotReachable,
			Message: `upstream host "h" unreachable after 3 attempt(s)`,
		}, true},
		{"a host connected before the dial failure", unreachable(false), plan.Answer{
			Status: 502, Type: "server_error", Code: "upstream_unreachable", Message: `upstream host "h" unreachable after 3 attempt(s)`,
		}, true},
		{"no credentials", pipeline.ErrNoKeys, plan.Answer{
			Status: 503, Type: "server_error", Code: "no_keys", Kind: plan.RefusalHostNotReachable, Message: "no host keys",
		}, true},
		{"upstream answered", &pipeline.UpstreamFailureError{Status: 500, Cause: pipeline.ErrAllKeysExhausted}, plan.Answer{}, false},
		{"caller gone", context.Canceled, plan.Answer{}, false},
		{"nil", nil, plan.Answer{}, false},
	} {
		got, ok := pipeline.AnswerOf(tc.err)
		if d := cmp.Diff(tc.want, got); d != "" || ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v (-want +got):\n%s", tc.name, ok, tc.ok, d)
		}
		if kind := pipeline.RefusalOf(tc.err); kind != tc.want.Kind {
			t.Errorf("%s: RefusalOf = %q, want %q", tc.name, kind, tc.want.Kind)
		}
	}
}

// A dial failure is relay's host_not_reachable refusal only when no candidate accepted a connection; a host that answered before the last one failed to dial leaves it an upstream failure.
func TestRun_UnreachableIsARefusalOnlyWhenNoCandidateConnected(t *testing.T) {
	t.Parallel()
	down := &host.Host{Meta: meta.Metadata{ID: "down", Name: "down"}, Spec: host.Spec{BaseURL: "http://down"}}
	up := &host.Host{Meta: meta.Metadata{ID: "up", Name: "up"}, Spec: host.Spec{BaseURL: "http://up"}}
	for name, tc := range map[string]struct {
		hops      []plan.RouteHop
		want      plan.RefusalKind
		wantCalls []string
	}{
		"every host down": {[]plan.RouteHop{hopAt(down, []*hostkey.HostKey{makeKey("d1", "sk-d1")})}, plan.RefusalHostNotReachable,
			[]string{"sk-d1", "sk-d1", "sk-d1"}},
		"an earlier host answered": {[]plan.RouteHop{
			hopAt(up, []*hostkey.HostKey{makeKey("u2", "sk-u2")}),
			hopAt(down, []*hostkey.HostKey{makeKey("d2", "sk-d2")}),
		}, "", []string{"sk-u2", "sk-d2", "sk-d2", "sk-d2"}},
	} {
		adp := newKeyedAdapter()
		adp.status["sk-u2"] = 500
		adp.errs["sk-d1"], adp.errs["sk-d2"] = errDial, errDial
		_, err := newPipeline().Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: tc.hops})
		var unreachable *pipeline.UpstreamUnreachableError
		if !errors.As(err, &unreachable) || unreachable.Host != "down" {
			t.Fatalf("%s: err = %v, want *UpstreamUnreachableError for host down", name, err)
		}
		if got := pipeline.RefusalOf(err); got != tc.want {
			t.Errorf("%s: RefusalOf = %q, want %q", name, got, tc.want)
		}
		if d := cmp.Diff(tc.wantCalls, adp.calls); d != "" {
			t.Errorf("%s: credentials called (-want +got):\n%s", name, d)
		}
	}
}

// An upstream path the host's backend does not fill is the hop's configuration fault: the walk moves to the next hop at once, with no re-dial, no other credential of the hop, no key failure and no reachability write.
func TestRun_UnfilledPathMovesToTheNextHop(t *testing.T) {
	t.Parallel()
	unfilled := &host.Host{Meta: meta.Metadata{ID: "unfilled", Name: "unfilled"}, Spec: host.Spec{BaseURL: "http://unfilled"}}
	served := &host.Host{Meta: meta.Metadata{ID: "served", Name: "served"}, Spec: host.Spec{BaseURL: "http://served"}}
	for name, tc := range map[string]struct {
		hops      []plan.RouteHop
		wantCalls []string
	}{
		"a later hop serves": {[]plan.RouteHop{
			hopAt(unfilled, []*hostkey.HostKey{makeKey("p1", "sk-p1"), makeKey("p2", "sk-p2")}),
			hopAt(served, []*hostkey.HostKey{makeKey("s1", "sk-s1")}),
		}, []string{"sk-p1", "sk-s1"}},
		"no hop left": {[]plan.RouteHop{
			hopAt(unfilled, []*hostkey.HostKey{makeKey("p3", "sk-p3"), makeKey("p4", "sk-p4")}),
		}, []string{"sk-p3"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			adp := newKeyedAdapter()
			for _, k := range []string{"sk-p1", "sk-p2", "sk-p3", "sk-p4"} {
				adp.errs[k] = &pipeline.PathPlaceholderError{Path: "/v1/{project}", Key: "project"}
			}
			agent := &scriptedAgent{}
			health := make(hostHealthLog, 4)
			p := newPipeline()
			p.KeyAgent, p.HostHealth = agent, health
			res, err := p.Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: tc.hops})
			if len(tc.hops) > 1 {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				drainResult(t, res)
				if got := <-health; got != "reachable:served" {
					t.Errorf("reachability write = %q, want reachable:served", got)
				}
			} else if _, ok := errors.AsType[*pipeline.PathPlaceholderError](err); !ok {
				t.Fatalf("err = %v, want *pipeline.PathPlaceholderError", err)
			}
			if d := cmp.Diff(tc.wantCalls, adp.calls); d != "" {
				t.Errorf("credentials called (-want +got):\n%s", d)
			}
			if len(agent.asked) != 0 {
				t.Errorf("key agent consulted on %v; an unfilled path is no credential's failure", agent.asked)
			}
		})
	}
}

// hostHealthLog records reachability writes as "reachable:<host>" or "unreachable:<host>".
type hostHealthLog chan string

func (h hostHealthLog) Reachable(_ context.Context, hostID string) { h <- "reachable:" + hostID }
func (h hostHealthLog) Unreachable(_ context.Context, hostID, _ string) {
	h <- "unreachable:" + hostID
}

// Reachability is the host's: any HTTP answer marks it reachable, also one every credential failed on; only dial failures mark it unreachable.
func TestRun_RecordsHostReachability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{"served", 0, nil, "reachable:host-test"},
		{"every credential rejected", 401, nil, "reachable:host-test"},
		{"dial failure", 0, errDial, "unreachable:host-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := kv.NewMem()
			t.Cleanup(func() { _ = mem.Close() })
			health := make(hostHealthLog, 4)
			p := &pipeline.Pipeline{Limiter: limiterOver(mem), Selector: keypool.New(mem, slog.Default(), nil, nil), Logger: slog.Default(), HostHealth: health}
			adp := newKeyedAdapter()
			adp.status["sk-1"], adp.errs["sk-1"] = tc.status, tc.err
			res, err := p.Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1")})})
			if (err == nil) != (tc.status == 0 && tc.err == nil) {
				t.Fatalf("Run: %v", err)
			}
			if res != nil {
				select {
				case got := <-health:
					t.Fatalf("host health %q written before the body closed", got)
				default:
				}
				drainResult(t, res)
			}
			select {
			case got := <-health:
				if got != tc.want {
					t.Errorf("host health = %q, want %q", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no host health write")
			}
		})
	}
}

// Post-flight starts when the caller closes the body, not before, and its observers see the served status, the whole body, the credential and the timing marks in order.
func TestRun_PostFlightFiresOnBodyClose(t *testing.T) {
	t.Parallel()
	type seen struct {
		status       int
		body, keyID  string
		attempts     int
		timing       lifecycle.Timing
		streamed     bool
		responseCode int
	}
	fired := make(chan seen, 2)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		fired <- seen{ev.Status, string(ev.ResponseBody), lc.HostKeyID, lc.Attempts, lc.Timing, lc.Streamed, lc.ResponseStatus}
		return nil, nil
	}})
	p := newPipeline()
	p.Lifecycle = reg
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:   &fakeAdapter{},
		Hops:      hopOf([]*hostkey.HostKey{makeKey("h1", "sk-1")}),
		Lifecycle: lifecycle.NewContext("req", "pipeline", time.Now()),
	})
	if err != nil || res.Status != 200 || res.KeyHash != "h1" {
		t.Fatalf("Run = %+v, %v; want 200 served by h1", res, err)
	}
	body, _ := io.ReadAll(res.Body)
	select {
	case <-fired:
		t.Fatal("post-flight fired before Body.Close")
	default:
	}
	if err := res.Body.Close(); err != nil || string(body) != "ok" {
		t.Fatalf("body = %q, Close: %v", body, err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	var got seen
	select {
	case got = <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never fired")
	}
	tm := got.timing
	got.timing = lifecycle.Timing{}
	if want := (seen{status: 200, body: "ok", keyID: "h1", attempts: 1, responseCode: 200}); got != want {
		t.Errorf("post-flight saw %+v, want %+v", got, want)
	}
	if !(0 < tm.Upstream.Start && tm.Upstream.Start <= tm.Upstream.ResponseStart && tm.Upstream.ResponseStart <= tm.Upstream.ResponseEnd && tm.Upstream.ResponseEnd <= tm.End) {
		t.Errorf("timing marks out of order: %+v", tm)
	}
	select {
	case again := <-fired:
		t.Errorf("post-flight fired twice: %+v", again)
	case <-time.After(20 * time.Millisecond):
	}
}

// The adapter is told whether the credential it is handed is an OAuth token, so it can pick that auth variant.
func TestRun_TellsTheAdapterAnOAuthCredential(t *testing.T) {
	t.Parallel()
	for kind, want := range map[hostkey.ValueKind]bool{
		hostkey.ValueKindOAuth: true, hostkey.ValueKindStored: false, hostkey.ValueKindEnv: false, "": false,
	} {
		key := makeKey("h", "sk-abc")
		key.Spec.ValueFrom.Kind = kind
		adp := &fakeAdapter{}
		res, err := newPipeline().Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: hopOf([]*hostkey.HostKey{key})})
		if err != nil {
			t.Fatalf("kind %q: Run: %v", kind, err)
		}
		drainResult(t, res)
		if got := adp.lastOAuth.Load(); got != want {
			t.Errorf("oauth flag for kind %q = %v, want %v", kind, got, want)
		}
	}
}
