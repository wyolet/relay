package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// A metered round-robin request reaches the upstream after one kv round
// trip: the inbound reserve, the breaker reads and the rotation counter go
// out together.
func TestRunAdmissionIsOneRoundTrip(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	store := kvtest.NewRecording(mem)

	rl := &appratelimit.RateLimit{
		Meta: meta.Metadata{ID: "rl-inbound", Name: "inbound"},
		Spec: appratelimit.Spec{Rules: []appratelimit.Rule{{
			Meter: appratelimit.MeterRequests, Amount: 100,
			Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
		}}},
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
	p := &Pipeline{
		Policy: policy.NewService(snap, keypool.New(store, slog.Default(), nil, nil), pkgratelimit.New(store, slog.Default(), nil)),
		Logger: slog.Default(),
	}

	var tripsAtCall int
	res, err := p.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			tripsAtCall = store.RoundTrips()
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
		}},
		Keys:   keys,
		Policy: caller,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = res.Body.Close()
	if tripsAtCall != 1 {
		t.Fatalf("kv round trips before the upstream call = %d, want 1", tripsAtCall)
	}
}
