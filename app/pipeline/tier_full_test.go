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

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
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
	snap  *reservationTestSnapshot
	store *kvtest.Recording
	keys  []*hostkey.HostKey
	used  []string
	done  hostReached
}

// newTierFullHarness builds n prioritized keys. The first full keys sit on a
// tier that allows one request a minute; the rest on a tier with room and a
// token meter, so a served request also commits post-flight.
func newTierFullHarness(t *testing.T, n, full int) *tierFullHarness {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	store := kvtest.NewRecording(mem)

	minute := appratelimit.Window(time.Minute)
	small := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-small", Name: "small"}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1, Window: minute, Strategy: appratelimit.StrategyFixedWindow},
	}}}
	big := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-big", Name: "big"}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1000, Window: minute, Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokens, Amount: 1 << 20, Window: minute, Strategy: appratelimit.StrategySlidingWindow},
	}}}
	inbound := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-inbound", Name: "inbound"}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1000, Window: minute, Strategy: appratelimit.StrategyFixedWindow},
	}}}
	tierSmall := &policy.Policy{Meta: meta.Metadata{ID: "pol-small", Name: "tier-small"}, Spec: policy.Spec{RateLimitID: small.Meta.ID}}
	tierBig := &policy.Policy{Meta: meta.Metadata{ID: "pol-big", Name: "tier-big"}, Spec: policy.Spec{RateLimitID: big.Meta.ID}}
	snap := &reservationTestSnapshot{
		policies:   map[string]*policy.Policy{tierSmall.Meta.ID: tierSmall, tierBig.Meta.ID: tierBig},
		rateLimits: map[string]*appratelimit.RateLimit{small.Meta.ID: small, big.Meta.ID: big, inbound.Meta.ID: inbound},
	}
	h := &tierFullHarness{snap: snap, store: store, done: make(hostReached, 16)}
	for i := range n {
		id := "key-" + string(rune('1'+i))
		tier := tierBig.Meta.ID
		if i < full {
			tier = tierSmall.Meta.ID
		}
		h.keys = append(h.keys, &hostkey.HostKey{
			Meta: meta.Metadata{ID: id, Name: id}, Spec: hostkey.Spec{PolicyID: tier},
			Resolved: "sk-" + id, KeyHash: "hash-" + id,
		})
	}
	h.p = h.newPipeline()
	return h
}

// newPipeline is a pod of its own: same kv, nothing remembered.
func (h *tierFullHarness) newPipeline() *Pipeline {
	return &Pipeline{
		Policy:     policy.NewService(h.snap, keypool.New(h.store, slog.Default(), nil, nil), pkgratelimit.New(h.store, slog.Default(), nil)),
		Logger:     slog.Default(),
		HostHealth: h.done,
	}
}

func (h *tierFullHarness) run(t *testing.T) (*lifecycle.Context, error) {
	t.Helper()
	return h.runOn(t, h.p)
}

func (h *tierFullHarness) runOn(t *testing.T, p *Pipeline) (*lifecycle.Context, error) {
	t.Helper()
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	res, err := p.Run(context.Background(), &Request{
		Adapter: keyRecordingAdapter{used: &h.used},
		Keys:    h.keys,
		Policy: &policy.Policy{
			Meta: meta.Metadata{ID: "pol-caller", Name: "caller"},
			Spec: policy.Spec{RateLimitID: "rl-inbound", KeySelection: policy.KeySelectionPrioritized},
		},
		Model:     &model.Model{Meta: meta.Metadata{ID: "model-1", Name: "m1"}},
		Host:      &host.Host{Meta: meta.Metadata{ID: "host-1", Name: "upstream"}},
		Lifecycle: lc,
	})
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

// fill spends the one request the small tier allows on each of the first
// full keys, from another pod, so h.p has not seen them refuse.
func (h *tierFullHarness) fill(t *testing.T, full int) {
	t.Helper()
	other := h.newPipeline()
	for i := range full {
		if _, err := h.runOn(t, other); err != nil {
			t.Fatalf("fill request %d: %v", i, err)
		}
	}
	if len(h.used) != full || h.used[full-1] != h.keys[full-1].Resolved {
		t.Fatalf("fill used %v, want the first %d keys", h.used, full)
	}
	h.used = nil
}

// With the first three prioritized keys over their tier, the request is served
// on the fourth in one attempt: refused tier reserves spend no attempt, so the
// default budget of three no longer runs out on keys that never reached the
// upstream. The next request skips the full keys without asking kv about them.
func TestRunServesPastFullTiers(t *testing.T) {
	h := newTierFullHarness(t, 5, 3)
	h.fill(t, 3)
	reservesBefore := len(h.store.Named("limit.reserve"))

	lc, err := h.run(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != "sk-key-4" {
		t.Fatalf("upstream calls used %v, want one on key-4", h.used)
	}
	if lc.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", lc.Attempts)
	}
	if got := len(h.store.Named("limit.reserve")) - reservesBefore; got != 5 {
		t.Fatalf("reserve scripts = %d, want 5 (inbound, three refused, key-4's)", got)
	}

	// Learned: the next request goes straight to key-4 in the healthy count of
	// trips: the admit batch, key-4's tier reserve, the post-flight commit.
	before := h.store.RoundTrips()
	h.used = nil
	if _, err := h.run(t); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(h.used) != 1 || h.used[0] != "sk-key-4" {
		t.Fatalf("second request used %v, want key-4", h.used)
	}
	if trips := h.store.RoundTrips() - before; trips != 3 {
		t.Fatalf("kv round trips = %d, want 3 (%v)", trips, h.store.Batches())
	}
}

// With every key over its tier the request is refused as rate limited, with
// the earliest time a key has room, and never reaches the upstream.
func TestRunAllTiersFull(t *testing.T) {
	h := newTierFullHarness(t, 3, 3)
	h.fill(t, 3)

	for i := range 2 {
		_, err := h.run(t)
		var exceeded *pkgratelimit.KeyQuotaExhausted
		if !errors.Is(err, policy.ErrKeyTiersFull) || !errors.As(err, &exceeded) {
			t.Fatalf("request %d: err = %v, want ErrKeyTiersFull carrying the refusal", i, err)
		}
		if exceeded.RetryAfter <= 0 || exceeded.RetryAfter > time.Minute {
			t.Fatalf("request %d: retry after %v, want within the window", i, exceeded.RetryAfter)
		}
		if kind, status := classifyFailure(err); kind != "rate_limited" || status != 0 {
			t.Fatalf("request %d: usage kind %q status %d, want a refusal (rate_limited, 0)", i, kind, status)
		}
	}
	if len(h.used) != 0 {
		t.Fatalf("upstream was called with %v", h.used)
	}
}
