package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// oneRequestPerHour is a token bucket a cancelled reservation refunds.
var oneRequestPerHour = appratelimit.Rule{
	Meter: appratelimit.MeterRequests, Amount: 1, Window: appratelimit.Window(time.Hour),
	Strategy: appratelimit.StrategyTokenBucket,
}

var concurrency = appratelimit.Rule{
	Meter: appratelimit.MeterConcurrency, Amount: 10, Window: appratelimit.Window(time.Minute),
	Strategy: appratelimit.StrategySlidingWindow,
}

// meteredPipeline is a Pipeline whose limiter counts on a recorded store, and the caller counters of rules (default: oneRequestPerHour).
func meteredPipeline(t *testing.T, rules ...appratelimit.Rule) (*Pipeline, *kvtest.Recording, []limits.Counter) {
	t.Helper()
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	store := kvtest.NewRecording(mem)
	if len(rules) == 0 {
		rules = []appratelimit.Rule{oneRequestPerHour}
	}
	rl := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-1", Name: "one-per-hour"}, Spec: appratelimit.Spec{Rules: rules}}
	var cs []limits.Counter
	for _, r := range rules {
		cs = append(cs, limits.CallerRule(limits.RungLane, rl, r, "pol-1", "account:a", "account:a"))
	}
	return New(lua.New(pkgratelimit.New(store, slog.New(slog.DiscardHandler), nil)), nil, nil), store, cs
}

// withPostFlight moves p's settles to a second recorded view of shared's store.
func withPostFlight(t *testing.T, p *Pipeline, shared *kvtest.Recording) *kvtest.Recording {
	t.Helper()
	mem, ok := shared.ScriptStore.(*kv.Mem)
	if !ok {
		t.Fatal("meteredPipeline store is not a *kv.Mem")
	}
	post := kvtest.NewRecording(mem)
	p.Limiter = lua.New(pkgratelimit.New(shared, slog.New(slog.DiscardHandler), nil)).WithPostFlight(pkgratelimit.New(post, slog.New(slog.DiscardHandler), nil))
	return post
}

func meteredRequest(baseURL string, cs []limits.Counter) *Request {
	return &Request{
		Method:       http.MethodPost,
		Path:         "/v1/chat/completions",
		Body:         strings.NewReader(`{}`),
		Headers:      http.Header{},
		HostBaseURL:  baseURL,
		UpstreamAuth: "Bearer upstream-key",
		Counters:     cs,
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

func TestRunSettlesReservationOnBodyClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	// The concurrency slot is what the settle has to release.
	p, store, cs := meteredPipeline(t, oneRequestPerHour, concurrency)

	res, err := p.Run(context.Background(), meteredRequest(upstream.URL, cs))
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

	// The settle kept the charge: the bucket's one request is spent.
	_, err = p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	var refused *limits.Refused
	if !errors.As(err, &refused) || refused.Counter.PolicyID != "pol-1" {
		t.Fatalf("second Run: err = %v, want a refusal naming pol-1", err)
	}
}

// A served request settles through the post-flight limiter; the hold stays on the shared one.
func TestRunCommitsOnThePostFlightStore(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p, shared, cs := meteredPipeline(t, oneRequestPerHour, concurrency)
	post := withPostFlight(t, p, shared)

	res, err := p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	awaitCommits(t, post, 1)
	if n := len(shared.Named("limit.reserve")); n != 1 {
		t.Fatalf("shared reserves = %d, want 1", n)
	}
	if n := len(shared.Named("limit.commit")); n != 0 {
		t.Fatalf("shared commits = %d, want none", n)
	}
}

// postFlightsFinished counts finished post-flight goroutines: the runner records post_flight_seconds as its last act, after any commit.
func postFlightsFinished(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.PostFlightSeconds.Write(&m); err != nil {
		t.Fatalf("read post_flight_seconds: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func awaitPostFlight(t *testing.T, before uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for postFlightsFinished(t) == before {
		if time.Now().After(deadline) {
			t.Fatal("post-flight never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// A requests-only reservation was settled by the hold: a successful request keeps its charge and post-flight sends no commit.
func TestRunRequestOnlyReservationSendsNoCommit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p, store, cs := meteredPipeline(t)

	res, err := p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	before := postFlightsFinished(t)
	_ = res.Body.Close()
	awaitPostFlight(t, before)
	_, err = p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	var refused *limits.Refused
	if !errors.As(err, &refused) {
		t.Fatalf("second Run: err = %v, want a refusal", err)
	}
	if got := len(store.Named("limit.commit")); got != 0 {
		t.Fatalf("commit scripts = %d, want none for a requests-only reservation", got)
	}
}

// A failed request's hold is released before Run returns, on the post-flight store: the refunded bucket admits the retry.
// A refused proxy request's row records what the caller is answered: 429 and the client message, never the counter key.
func TestRunRefusalRowRecordsTheAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p, _, cs := meteredPipeline(t)
	got := make(chan *lifecycle.PostFlightEvent, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		if ev.Refusal != "" {
			got <- ev
		}
		return nil, nil
	}})
	p.Lifecycle = reg
	res, err := p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	req := meteredRequest(upstream.URL, cs)
	req.Lifecycle = lifecycle.NewContext("refused", "proxy", time.Now())
	if _, err := p.Run(context.Background(), req); err == nil {
		t.Fatal("second Run was not refused")
	}
	select {
	case ev := <-got:
		if ev.Status != http.StatusTooManyRequests || ev.Refusal != "no_capacity" || !strings.HasPrefix(ev.ErrorMessage, "the rate limit of the lane policy pol-1 is exhausted; try again in ") {
			t.Fatalf("row status %d refusal %q message %q", ev.Status, ev.Refusal, ev.ErrorMessage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no failure event")
	}
}

func TestRunRefundsOnThePostFlightStore(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	p, shared, cs := meteredPipeline(t)
	post := withPostFlight(t, p, shared)

	for i := range 2 {
		_, err := p.Run(context.Background(), meteredRequest(deadURL, cs))
		var refused *limits.Refused
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("Run %d: err = %v, want the upstream failure (a refunded bucket admits the retry)", i, err)
		}
		if n, s := len(post.Named("limit.commit")), len(shared.Named("limit.commit")); n != i+1 || s != 0 {
			t.Fatalf("Run %d: commits = %d post-flight, %d shared; want %d, 0", i, n, s, i+1)
		}
	}
}

// lateRefusal is a request body read to its end whose refusal comes once the upstream has answered, as a body checked while it streams can be.
type lateRefusal struct {
	io.Reader
	refused *BodyError
}

func (b *lateRefusal) Refusal() *BodyError { return b.refused }

// A body refused after the upstream answered still keeps the request's charge: the upstream served it, so releasing the hold would refund what was spent.
func TestRunKeepsTheChargeWhenTheBodyIsRefusedAfterTheAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p, _, cs := meteredPipeline(t)

	req := meteredRequest(upstream.URL, cs)
	body := &lateRefusal{Reader: strings.NewReader(`{}`)}
	req.Body = body
	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	body.refused = &BodyError{Kind: "duplicate_model", Err: errors.New("second model key")}
	before := postFlightsFinished(t)
	_ = res.Body.Close()
	awaitPostFlight(t, before)

	_, err = p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	var refused *limits.Refused
	if !errors.As(err, &refused) {
		t.Fatalf("second Run: err = %v, want a refusal: the first request's charge was refunded", err)
	}
}

// failingBody sends its first part, then fails: a body refused after the request went out.
type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, &BodyError{Kind: "duplicate_model", Err: errors.New("second model key")}
	}
	b.sent = true
	return copy(p, `{"model":"m",`), nil
}

// A request that failed after it was written to the upstream keeps its charge; one that never reached it (TestRunRefundsOnThePostFlightStore) is refunded.
func TestRunKeepsTheChargeOnceTheRequestWentOut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer upstream.Close()
	p, _, cs := meteredPipeline(t)

	req := meteredRequest(upstream.URL, cs)
	req.Body = &failingBody{}
	var bodyErr *BodyError
	if _, err := p.Run(context.Background(), req); !errors.As(err, &bodyErr) {
		t.Fatalf("Run: err = %v, want the body's refusal", err)
	}
	_, err := p.Run(context.Background(), meteredRequest(upstream.URL, cs))
	var refused *limits.Refused
	if !errors.As(err, &refused) {
		t.Fatalf("second Run: err = %v, want a refusal: the first request's charge was refunded", err)
	}
}

// A hold the store could not answer wraps the store's timeout; its usage row must still read as a kv refusal, not as a timeout.
func TestClassifyFailureStoreUnavailable(t *testing.T) {
	err := fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded))
	if kind, status := classifyFailure(err); kind != "kv_unavailable" || status != 0 {
		t.Fatalf("classifyFailure = %q, %d; want kv_unavailable, 0", kind, status)
	}
}

func TestRunWithCountersAndNoLimiterRefuses(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	_, cs := func() (*Pipeline, []limits.Counter) { p, _, cs := meteredPipeline(t); return p, cs }()
	_, err := New(nil, nil, nil).Run(context.Background(), meteredRequest(upstream.URL, cs))
	if !errors.Is(err, ErrLimiterMissing) {
		t.Fatalf("err = %v, want ErrLimiterMissing", err)
	}
	if hits != 0 {
		t.Fatal("an unmetered request reached the upstream")
	}
}
