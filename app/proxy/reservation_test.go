package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

type policySnap struct {
	pol *policy.Policy
	rl  *appratelimit.RateLimit
}

func (s policySnap) Policy(context.Context, string) (*policy.Policy, bool) { return s.pol, true }
func (s policySnap) RateLimit(_ context.Context, id string) (*appratelimit.RateLimit, bool) {
	return s.rl, s.rl.Meta.ID == id
}

// oneRequestPerHour is a token bucket a cancelled reservation refunds.
var oneRequestPerHour = appratelimit.Rule{
	Meter: appratelimit.MeterRequests, Amount: 1, Window: appratelimit.Window(time.Hour),
	Strategy: appratelimit.StrategyTokenBucket,
}

// authedPipeline is a Pipeline whose policy service meters rules (default:
// oneRequestPerHour).
func authedPipeline(t *testing.T, rules ...appratelimit.Rule) (*Pipeline, *kvtest.Recording, *policy.InboundInput) {
	t.Helper()
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	store := kvtest.NewRecording(mem)

	if len(rules) == 0 {
		rules = []appratelimit.Rule{oneRequestPerHour}
	}
	rl := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-1", Name: "one-per-hour"}}
	rl.Spec.Rules = rules
	pol := &policy.Policy{Meta: meta.Metadata{ID: "pol-1", Name: "p"}, Spec: policy.Spec{RateLimitID: rl.Meta.ID}}
	limiter := pkgratelimit.New(store, slog.New(slog.DiscardHandler), nil)

	p := New(limiter, nil, nil)
	p.Policy = policy.NewService(policySnap{pol: pol, rl: rl}, nil, limiter)
	return p, store, &policy.InboundInput{Policy: pol}
}

func authedRequest(baseURL string, in *policy.InboundInput) *Request {
	return &Request{
		Method:       http.MethodPost,
		Path:         "/v1/chat/completions",
		Body:         strings.NewReader(`{}`),
		Headers:      http.Header{},
		HostBaseURL:  baseURL,
		UpstreamAuth: "Bearer upstream-key",
		Inbound:      in,
	}
}

func awaitCommits(t *testing.T, store *kvtest.Recording, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.Named("limit.commit")) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("commit scripts = %d, want %d", len(store.Named("limit.commit")), want)
}

func TestRunCommitsInboundReservationOnBodyClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	// The concurrency slot is what the commit has to release.
	p, store, in := authedPipeline(t, oneRequestPerHour, appratelimit.Rule{
		Meter: appratelimit.MeterConcurrency, Amount: 10, Window: appratelimit.Window(time.Minute),
		Strategy: appratelimit.StrategySlidingWindow,
	})

	res, err := p.Run(context.Background(), authedRequest(upstream.URL, in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.Named("limit.reserve")); got != 1 {
		t.Fatalf("reserve scripts = %d, want 1", got)
	}
	if got := len(store.Named("limit.commit")); got != 0 {
		t.Fatalf("committed before the body closed: %d", got)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	awaitCommits(t, store, 1)

	// The commit kept the charge: the bucket's one request is spent.
	_, err = p.Run(context.Background(), authedRequest(upstream.URL, in))
	var exceeded *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &exceeded) {
		t.Fatalf("second Run: err = %v, want KeyQuotaExhausted", err)
	}
}

// A requests-only reservation was settled by the reserve: a successful
// request keeps its charge and post-flight sends no commit.
func TestRunRequestOnlyReservationSendsNoCommit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p, store, in := authedPipeline(t)

	res, err := p.Run(context.Background(), authedRequest(upstream.URL, in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	_, err = p.Run(context.Background(), authedRequest(upstream.URL, in))
	var exceeded *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &exceeded) {
		t.Fatalf("second Run: err = %v, want KeyQuotaExhausted", err)
	}
	// Post-flight is detached; a commit it was going to send would land well
	// inside this wait.
	time.Sleep(100 * time.Millisecond)
	if got := len(store.Named("limit.commit")); got != 0 {
		t.Fatalf("commit scripts = %d, want none for a requests-only reservation", got)
	}
}

func TestRunRefundsInboundReservationOnUpstreamFailure(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	p, store, in := authedPipeline(t)

	for i := range 2 {
		_, err := p.Run(context.Background(), authedRequest(deadURL, in))
		var exceeded *pkgratelimit.KeyQuotaExhausted
		if err == nil || errors.As(err, &exceeded) {
			t.Fatalf("Run %d: err = %v, want the upstream failure (a refunded bucket admits the retry)", i, err)
		}
	}
	if got := len(store.Named("limit.commit")); got != 2 {
		t.Fatalf("commit scripts = %d, want one refund per failed request", got)
	}
}

func TestRunAuthedWithoutPolicyServiceRefuses(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	_, err := New(nil, nil, nil).Run(context.Background(), authedRequest(upstream.URL, &policy.InboundInput{}))
	if !errors.Is(err, ErrPolicyMissing) {
		t.Fatalf("err = %v, want ErrPolicyMissing", err)
	}
	if hits != 0 {
		t.Fatal("an unmetered authed request reached the upstream")
	}
}
