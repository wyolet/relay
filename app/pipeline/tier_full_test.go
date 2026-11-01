package pipeline

import (
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
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// keyRecordingAdapter answers 200 and records which key each call used.
type keyRecordingAdapter struct {
	reservationTestAdapter
	used *[]string
}

func (a keyRecordingAdapter) Call(_ context.Context, _ string, _ *string, key string, _ []byte, _ http.Header, _ string, _ bool, _ bool) (*http.Response, error) {
	*a.used = append(*a.used, key)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
}

type tierFullHarness struct {
	p     *Pipeline
	store *kvtest.Recording
	keys  []*hostkey.HostKey
	full  int
	hops  []plan.RouteHop
	used  []string
	done  hostReached
}

// newTierFullHarness builds n prioritized keys. The first full keys share one account whose tier allows one request a minute; the rest share an account with room and a token meter, so a served request also settles post-flight.
func newTierFullHarness(t *testing.T, n, full int) *tierFullHarness {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	h := &tierFullHarness{store: kvtest.NewRecording(mem), full: full, done: make(hostReached, 16)}
	for i := range n {
		h.keys = append(h.keys, testKey("key-"+string(rune('1'+i))))
	}
	h.p = h.newPipeline()
	h.hops = h.newHops()
	return h
}

// newHops is one pod's plans: the account cooldowns live on them, so a pod built with its own has seen nothing.
func (h *tierFullHarness) newHops() []plan.RouteHop {
	small := perMinute("rl-small", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 1)
	big := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-big", Name: "big"}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1000, Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokens, Amount: 1 << 20, Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategySlidingWindow},
	}}}
	inbound := perMinute("rl-inbound", appratelimit.MeterRequests, appratelimit.StrategyFixedWindow, 1000)
	accSmall, accBig := accountPlan("acct-small"), accountPlan("acct-big")
	var slots []testSlot
	for i, k := range h.keys {
		s := testSlot{key: k, acc: accBig, upstream: accountCounters("acct-big", big)}
		if i < h.full {
			s = testSlot{key: k, acc: accSmall, upstream: accountCounters("acct-small", small)}
		}
		slots = append(slots, s)
	}
	return []plan.RouteHop{buildHop(keypool.KeySelectionPrioritized, callerCounter(inbound), slots...)}
}

// newPipeline is a pod's pipeline over the shared kv.
func (h *tierFullHarness) newPipeline() *Pipeline {
	return &Pipeline{
		Limiter:    lua.New(pkgratelimit.New(h.store, slog.Default(), nil)),
		Selector:   keypool.New(h.store, slog.Default(), nil, nil),
		Logger:     slog.Default(),
		HostHealth: h.done,
	}
}

func (h *tierFullHarness) request(lc *lifecycle.Context, hops []plan.RouteHop) *Request {
	return &Request{
		Adapter:   keyRecordingAdapter{used: &h.used},
		Hops:      hops,
		Model:     &model.Model{Meta: meta.Metadata{ID: "model-1", Name: "m1"}},
		Lifecycle: lc,
	}
}

func (h *tierFullHarness) run(t *testing.T) (*lifecycle.Context, error) {
	t.Helper()
	return h.runOn(t, h.p, h.hops)
}

func (h *tierFullHarness) runOn(t *testing.T, p *Pipeline, hops []plan.RouteHop) (*lifecycle.Context, error) {
	t.Helper()
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	res, err := p.Run(context.Background(), h.request(lc, hops))
	if err != nil {
		return lc, err
	}
	_ = res.Body.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
	return lc, nil
}

// fill spends the small account's one request from another pod, so h.p has not seen it refuse.
func (h *tierFullHarness) fill(t *testing.T) {
	t.Helper()
	if _, err := h.runOn(t, h.newPipeline(), h.newHops()); err != nil {
		t.Fatalf("fill request: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != h.keys[0].Resolved {
		t.Fatalf("fill used %v, want the first key", h.used)
	}
	h.used = nil
}

// With the first three prioritized keys in a full account, the request is served on the fourth in one attempt: the account's refusal spends no attempt and cools the account, so its other keys are skipped without asking kv. The next request goes straight to key-4.
func TestRunServesPastFullAccount(t *testing.T) {
	h := newTierFullHarness(t, 5, 3)
	h.fill(t)
	reservesBefore := len(h.store.Named("limit.reserve"))

	lc, err := h.run(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != "sk-key-4" {
		t.Fatalf("upstream calls used %v, want one on key-4", h.used)
	}
	if lc.Attempts != 1 || lc.AccountID != "acct-big" {
		t.Fatalf("attempts = %d, account %q; want 1 on acct-big", lc.Attempts, lc.AccountID)
	}
	if got := len(h.store.Named("limit.reserve")) - reservesBefore; got != 3 {
		t.Fatalf("reserve scripts = %d, want 3 (caller, the full account's refusal, key-4's account)", got)
	}

	// The cooled account costs nothing: the caller hold, key-4's breaker read, its account hold, then the post-flight settle of the token meter.
	before := h.store.RoundTrips()
	h.used = nil
	if _, err := h.run(t); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != "sk-key-4" {
		t.Fatalf("second request used %v, want key-4", h.used)
	}
	if trips := h.store.RoundTrips() - before; trips != 4 {
		t.Fatalf("kv round trips = %d, want 4 (%v)", trips, h.store.Batches())
	}
}

// With every key in the full account the request is refused as rate limited, with the earliest time the account has room, and never reaches the upstream.
func TestRunAllAccountsFull(t *testing.T) {
	h := newTierFullHarness(t, 3, 3)
	h.fill(t)

	for i := range 2 {
		_, err := h.run(t)
		var limited *UpstreamLimitedError
		if !errors.As(err, &limited) {
			t.Fatalf("request %d: err = %v, want UpstreamLimitedError", i, err)
		}
		if limited.RetryAfter <= 0 || limited.RetryAfter > time.Minute {
			t.Fatalf("request %d: retry after %v, want within the window", i, limited.RetryAfter)
		}
		if kind, status := classifyFailure(err); kind != "rate_limited" || status != 0 {
			t.Fatalf("request %d: usage kind %q status %d, want a refusal (rate_limited, 0)", i, kind, status)
		}
	}
	if len(h.used) != 0 {
		t.Fatalf("upstream was called with %v", h.used)
	}
}
