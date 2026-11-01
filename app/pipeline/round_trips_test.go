package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// hostReached signals post-flight's last step, which runs after the settle.
type hostReached chan struct{}

func (h hostReached) Reachable(context.Context, string)         { h <- struct{}{} }
func (hostReached) Unreachable(context.Context, string, string) {}

// A round-robin request on a healthy key with only caller counters: before the upstream call, the caller hold and the pick's breaker reads with the rotation counter, one round trip each; the credential has no counters, so its hold costs none. Post-flight writes no success for a key that never failed, and settles in one more round trip only when a rule has something to settle.
func TestRunRoundTrips(t *testing.T) {
	requests := appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 100,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
	}
	tokens := appratelimit.Rule{
		Meter: appratelimit.MeterTokens, Amount: 1 << 20,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategySlidingWindow,
	}
	t.Run("requests_only", func(t *testing.T) { runRoundTrips(t, 2, requests) })
	t.Run("with_tokens", func(t *testing.T) { runRoundTrips(t, 3, requests, tokens) })
}

func runRoundTrips(t *testing.T, want int, rules ...appratelimit.Rule) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	store := kvtest.NewRecording(mem)

	rl := &appratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-inbound", Name: "inbound"}, Spec: appratelimit.Spec{Rules: rules}}
	hop := buildHop(keypool.KeySelectionRoundRobin, callerCounter(rl), testSlot{key: testKey("key-a")}, testSlot{key: testKey("key-b")})
	reached := make(hostReached, 1)
	p := &Pipeline{
		Limiter:    lua.New(pkgratelimit.New(store, slog.Default(), nil)),
		Selector:   keypool.New(store, slog.Default(), nil, nil),
		Logger:     slog.Default(),
		HostHealth: reached,
	}

	var tripsAtCall int
	res, err := p.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			tripsAtCall = store.RoundTrips()
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
		}},
		Hops: []plan.RouteHop{hop},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tripsAtCall != 2 {
		t.Fatalf("kv round trips before the upstream call = %d, want 2 (%v)", tripsAtCall, store.Batches())
	}
	_ = res.Body.Close()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
	if trips := store.RoundTrips(); trips != want {
		t.Fatalf("kv round trips for the request = %d, want %d (%d scripts: %v)", trips, want, len(store.Scripts()), store.Batches())
	}
	if n := len(store.Named("keypool.record_success")); n != 0 {
		t.Fatalf("success writes = %d, want none for a key that never failed", n)
	}
}
