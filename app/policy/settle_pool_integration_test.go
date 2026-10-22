//go:build integration

// settle_pool_integration_test.go covers Settle on its own kv client while the shared pool is exhausted.
// Run with: make test-integration.

package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/keypool"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// A served request's commit goes through the post-flight client, so it lands, token counts included, while admission finds no free connection on the shared pool.
func TestSettleLandsWhileSharedPoolExhausted(t *testing.T) {
	cfg := kvtest.Config(t)
	post := kvtest.Connect(t, cfg)
	shared := kvtest.ExhaustedPool(t, cfg)

	pol := fix("caller")
	pol.Meta.ID = "pol-1"
	rl := testRateLimit("rl-1", appratelimit.Rule{
		Meter: appratelimit.MeterTokens, Amount: 100,
		Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow,
	})
	pol.Spec.RateLimitID = rl.Meta.ID
	snap := reserveSnap{pol: pol, rl: rl}
	in := InboundInput{Policy: pol, TeamID: "team-1"}
	ctx := context.Background()

	svc := NewService(snap, keypool.New(shared, discardLogger(), nil, nil), pkgratelimit.New(shared, discardLogger(), nil)).
		WithPostFlight(keypool.New(post, discardLogger(), nil, nil), pkgratelimit.New(post, discardLogger(), nil))
	if _, err := svc.ReserveInbound(ctx, in); !errors.Is(err, pkgratelimit.ErrUnavailable) {
		t.Fatalf("reserve on the exhausted shared pool = %v, want ErrUnavailable", err)
	}

	// The reservation of a request admitted before the pool ran out.
	direct := NewService(snap, nil, pkgratelimit.New(post, discardLogger(), nil))
	res, err := direct.ReserveInbound(ctx, in)
	if err != nil || res == nil {
		t.Fatalf("ReserveInbound = %v, %v; want a reservation", res, err)
	}

	dropped := metrics.KVErrors.WithLabelValues("limit_commit", metrics.KVDropped)
	before := testutil.ToFloat64(dropped)
	obs := pkgratelimit.Observations{Tokens: map[string]int64{"input": 80, "output": 40}}
	if err := svc.Settle(ctx, res, nil, obs); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := testutil.ToFloat64(dropped) - before; got != 0 {
		t.Fatalf("limit_commit dropped moved by %v, want 0", got)
	}
	// The 120 tokens were counted: the 100-token budget is spent.
	if _, err := direct.ReserveInbound(ctx, in); !errors.Is(err, pkgratelimit.ErrExceeded) {
		t.Fatalf("reserve after the commit = %v, want ErrExceeded", err)
	}
}
