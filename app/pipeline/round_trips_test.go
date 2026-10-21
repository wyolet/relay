package pipeline

import (
	"context"
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
	"github.com/wyolet/relay/app/policy"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// hostReached signals post-flight's last step, which runs after the settle.
type hostReached chan struct{}

func (h hostReached) Reachable(context.Context, string)         { h <- struct{}{} }
func (hostReached) Unreachable(context.Context, string, string) {}

// A metered round-robin request on a healthy key: before the upstream call
// the inbound reserve, the breaker reads and the rotation counter go out in
// one round trip. Post-flight writes no success for a key that never failed,
// and commits in one more round trip only when a rule has something to
// settle (a requests-only reservation was settled by the reserve).
func TestRunRoundTrips(t *testing.T) {
	requests := appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 100,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
	}
	tokens := appratelimit.Rule{
		Meter: appratelimit.MeterTokens, Amount: 1 << 20,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategySlidingWindow,
	}
	t.Run("requests_only", func(t *testing.T) { runRoundTrips(t, 1, requests) })
	t.Run("with_tokens", func(t *testing.T) { runRoundTrips(t, 2, requests, tokens) })
}

func runRoundTrips(t *testing.T, want int, rules ...appratelimit.Rule) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	store := kvtest.NewRecording(mem)

	rl := &appratelimit.RateLimit{
		Meta: meta.Metadata{ID: "rl-inbound", Name: "inbound"},
		Spec: appratelimit.Spec{Rules: rules},
	}
	tier := &policy.Policy{Meta: meta.Metadata{ID: "pol-tier", Name: "tier"}}
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: "pol-caller", Name: "caller"},
		Spec: policy.Spec{RateLimitID: rl.Meta.ID, KeySelection: policy.KeySelectionRoundRobin},
	}
	snap := &reservationTestSnapshot{
		policies:   map[string]*policy.Policy{tier.Meta.ID: tier},
		rateLimits: map[string]*appratelimit.RateLimit{rl.Meta.ID: rl},
	}
	var keys []*hostkey.HostKey
	for _, id := range []string{"key-a", "key-b"} {
		keys = append(keys, &hostkey.HostKey{
			Meta: meta.Metadata{ID: id, Name: id}, Spec: hostkey.Spec{PolicyID: tier.Meta.ID},
			Resolved: "sk-" + id, KeyHash: "hash-" + id,
		})
	}
	reached := make(hostReached, 1)
	p := &Pipeline{
		Policy:     policy.NewService(snap, keypool.New(store, slog.Default(), nil, nil), pkgratelimit.New(store, slog.Default(), nil)),
		Logger:     slog.Default(),
		HostHealth: reached,
	}

	var tripsAtCall int
	res, err := p.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			tripsAtCall = store.RoundTrips()
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
		}},
		Keys:   keys,
		Policy: caller,
		Host:   &host.Host{Meta: meta.Metadata{ID: "host-1", Name: "upstream"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tripsAtCall != 1 {
		t.Fatalf("kv round trips before the upstream call = %d, want 1", tripsAtCall)
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
