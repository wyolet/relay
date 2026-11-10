package pipeline_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/meta"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// statusAdapter answers status for the calls failing reports (200 otherwise) and records each call as "key/model". It classifies like the spec adapter: 401, 403, 429 and 5xx are retryable; a 429 waits retryAfter.
type statusAdapter struct {
	mu         sync.Mutex
	calls      []string
	failing    func(key, upstreamModel string) bool
	status     int
	retryAfter time.Duration
}

func (a *statusAdapter) Call(_ context.Context, _ string, _ *string, key string, _ []byte, _ http.Header, upstreamModel string, _, _ bool) (*http.Response, error) {
	a.mu.Lock()
	a.calls = append(a.calls, key+"/"+upstreamModel)
	fail := a.failing != nil && a.failing(key, upstreamModel)
	a.mu.Unlock()
	if fail {
		return errResp(a.status), nil
	}
	return okResp("ok"), nil
}

func (a *statusAdapter) ExtractTokens([]byte) pkgusage.Tokens { return nil }

func (a *statusAdapter) Retryable(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if a.retryAfter > 5*time.Second {
			return true, keypool.FailureRateLimitLong, a.retryAfter
		}
		return true, keypool.FailureRateLimitShort, a.retryAfter
	case resp.StatusCode == http.StatusForbidden:
		return true, keypool.FailureForbidden, 0
	case resp.StatusCode >= 500:
		return true, keypool.FailureServerError, 0
	}
	return false, 0, 0
}

// take returns the calls since the last take.
func (a *statusAdapter) take() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := strings.Join(a.calls, " ")
	a.calls = nil
	return out
}

var (
	modelM = &model.Model{Meta: meta.Metadata{ID: "model-m", Name: "m"}}
	modelN = &model.Model{Meta: meta.Metadata{ID: "model-n", Name: "n"}}
)

// accountRoute is two hops: keys a and b of one account at one host, then key c of another account at a second host.
func accountRoute() (first plan.RouteHop, hops []plan.RouteHop) {
	first = withAccount(hopAt(testHost, []*hostkey.HostKey{makeKey("hash-a", "a"), makeKey("hash-b", "b")}), "acct-1")
	other := &host.Host{Meta: meta.Metadata{ID: "host-other", Name: "other"}}
	return first, []plan.RouteHop{first, withAccount(hopAt(other, []*hostkey.HostKey{makeKey("hash-c", "c")}), "acct-2")}
}

func runModel(t *testing.T, p *pipeline.Pipeline, a pipeline.Adapter, hops []plan.RouteHop, m *model.Model) (*pipeline.Result, error) {
	t.Helper()
	res, err := p.Run(context.Background(), &pipeline.Request{Adapter: a, Hops: hops, Model: m, UpstreamModel: m.Meta.Name})
	if err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	return res, err
}

// noBreakerRecord fails when the key's breaker gets a record within a short wait: breaker writes run detached.
func noBreakerRecord(t *testing.T, sel *keypool.Selector, keyHash string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if rec, found := sel.ReadCircuit(context.Background(), keyHash); found {
		t.Fatalf("breaker of %s was written: %+v", keyHash, rec)
	}
}

// A 429 on key a for model m cools the account for m: the same request moves to the other account's pool without calling key b, the next request for m skips the account, model n is still served by key a, and key a's breaker is untouched.
func TestUpstream429CoolsAccountForModel(t *testing.T) {
	t.Parallel()
	p := newPipeline()
	adp := &statusAdapter{status: http.StatusTooManyRequests, retryAfter: 30 * time.Second,
		failing: func(key, m string) bool { return key == "a" && m == "m" }}
	_, hops := accountRoute()

	res, err := runModel(t, p, adp, hops, modelM)
	if err != nil {
		t.Fatalf("Run m: %v", err)
	}
	if got := adp.take(); got != "a/m c/m" || res.KeyHash != "hash-c" {
		t.Fatalf("calls %q, served by %s; want a/m then c/m, served by hash-c", got, res.KeyHash)
	}
	if _, err := runModel(t, p, adp, hops, modelM); err != nil {
		t.Fatalf("second Run m: %v", err)
	}
	if got := adp.take(); got != "c/m" {
		t.Fatalf("second request for m called %q, want c/m only", got)
	}
	if _, err := runModel(t, p, adp, hops, modelN); err != nil {
		t.Fatalf("Run n: %v", err)
	}
	if got := adp.take(); got != "a/n" {
		t.Fatalf("request for n called %q, want a/n", got)
	}
	noBreakerRecord(t, p.Selector, "hash-a")
}

// With no other account to move to, the caller gets the upstream 429 without key b being called, and the next request is refused as limited until the cooldown ends, without an upstream call.
func TestUpstream429LastAccount(t *testing.T) {
	t.Parallel()
	p := newPipeline()
	adp := &statusAdapter{status: http.StatusTooManyRequests, retryAfter: 30 * time.Second,
		failing: func(string, string) bool { return true }}
	first, _ := accountRoute()
	hops := []plan.RouteHop{first}

	_, err := runModel(t, p, adp, hops, modelM)
	var upstream *pipeline.UpstreamFailureError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the upstream 429", err)
	}
	if got := adp.take(); got != "a/m" {
		t.Fatalf("calls %q, want a/m only", got)
	}
	_, err = runModel(t, p, adp, hops, modelM)
	var limited *pipeline.UpstreamLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 || limited.RetryAfter > 30*time.Second {
		t.Fatalf("second err = %v, want limited within the retry-after", err)
	}
	if got := adp.take(); got != "" {
		t.Fatalf("second request called %q, want none", got)
	}
}

// Once the retry-after has passed, the account serves the model again.
func TestUpstream429CooldownEnds(t *testing.T) {
	t.Parallel()
	p := newPipeline()
	var limitedNow sync.Mutex
	limiting := true
	adp := &statusAdapter{status: http.StatusTooManyRequests, retryAfter: 100 * time.Millisecond,
		failing: func(key, _ string) bool {
			limitedNow.Lock()
			defer limitedNow.Unlock()
			return limiting && key == "a"
		}}
	_, hops := accountRoute()

	if _, err := runModel(t, p, adp, hops, modelM); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := adp.take(); got != "a/m c/m" {
		t.Fatalf("calls %q, want a/m c/m", got)
	}
	limitedNow.Lock()
	limiting = false
	limitedNow.Unlock()
	time.Sleep(150 * time.Millisecond)
	res, err := runModel(t, p, adp, hops, modelM)
	if err != nil {
		t.Fatalf("Run after retry-after: %v", err)
	}
	if got := adp.take(); got != "a/m" || res.KeyHash != "hash-a" {
		t.Fatalf("calls %q served by %s, want a/m on hash-a", got, res.KeyHash)
	}
}

// A credential with no account keeps its own breaker: a long 429 opens it, its sibling serves, and the sibling's breaker is untouched.
func TestUpstream429WithoutAccountTripsBreaker(t *testing.T) {
	t.Parallel()
	p := newPipeline()
	adp := &statusAdapter{status: http.StatusTooManyRequests, retryAfter: 30 * time.Second,
		failing: func(key, _ string) bool { return key == "a" }}
	hops := hopOf([]*hostkey.HostKey{makeKey("hash-a", "a"), makeKey("hash-b", "b")})

	if _, err := runModel(t, p, adp, hops, modelM); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := adp.take(); got != "a/m b/m" {
		t.Fatalf("calls %q, want a/m b/m", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec, found := p.Selector.ReadCircuit(context.Background(), "hash-a")
		if found && rec.State == keypool.CircuitOpen && rec.Reason == keypool.ReasonUpstreamRateLimited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("breaker of hash-a = %+v (found %v), want open for the rate limit", rec, found)
		}
		time.Sleep(5 * time.Millisecond)
	}
	noBreakerRecord(t, p.Selector, "hash-b")
}

// An account relay grouped by owner and host does not declare one provider org: a 429 on key a trips a's breaker, key b in the same account serves, and the account does not cool.
func TestUpstream429InGroupedAccountTripsBreaker(t *testing.T) {
	t.Parallel()
	p := newPipeline()
	adp := &statusAdapter{status: http.StatusTooManyRequests, retryAfter: 30 * time.Second,
		failing: func(key, _ string) bool { return key == "a" }}
	first, _ := accountRoute()
	first.Slots[0].Account.GroupedByRelay = true
	hops := []plan.RouteHop{first}

	res, err := runModel(t, p, adp, hops, modelM)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := adp.take(); got != "a/m b/m" || res.KeyHash != "hash-b" {
		t.Fatalf("calls %q served by %s, want a/m b/m served by hash-b", got, res.KeyHash)
	}
	if first.Slots[0].Account.Cooldown.Cooled(time.Now().UnixMilli(), modelM.Meta.ID) {
		t.Fatal("the grouped account is cooling")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec, found := p.Selector.ReadCircuit(context.Background(), "hash-a")
		if found && rec.State == keypool.CircuitOpen && rec.Reason == keypool.ReasonUpstreamRateLimited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("breaker of hash-a = %+v (found %v), want open for the rate limit", rec, found)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Failures other than a rate limit never cool the account: a billing refusal (402) stops the request as before and the next one reaches key a again; a 403 moves on to key b in the same account.
func TestNonRateLimitFailureDoesNotCoolAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		calls  string
	}{
		{http.StatusPaymentRequired, "a/m"},
		{http.StatusForbidden, "a/m b/m"},
	} {
		p := newPipeline()
		adp := &statusAdapter{status: tc.status, failing: func(key, _ string) bool { return key == "a" }}
		first, _ := accountRoute()
		hops := []plan.RouteHop{first}

		_, _ = runModel(t, p, adp, hops, modelM)
		if got := adp.take(); got != tc.calls {
			t.Fatalf("%d: calls %q, want %q", tc.status, got, tc.calls)
		}
		if first.Slots[0].Account.Cooldown.Cooled(time.Now().UnixMilli(), modelM.Meta.ID) {
			t.Fatalf("%d: the account is cooling", tc.status)
		}
		if tc.status == http.StatusPaymentRequired {
			_, _ = runModel(t, p, adp, hops, modelM)
			if got := adp.take(); got != "a/m" {
				t.Fatalf("402: second request called %q, want a/m", got)
			}
		}
	}
}
