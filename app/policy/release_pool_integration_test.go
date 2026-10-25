//go:build integration

// release_pool_integration_test.go covers the failure writes on Redis and on their own kv client while the shared pool is exhausted.
// Run with: make test-integration.

package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func TestReleaseRedis(t *testing.T) {
	runReleaseSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

// A failed attempt's refund and key failure, and a failed request's settle, go through the post-flight client, so they land while admission finds no free connection on the shared pool.
func TestFailureWritesLandWhileSharedPoolExhausted(t *testing.T) {
	cfg := kvtest.Config(t)
	post := kvtest.Connect(t, cfg)
	shared := kvtest.ExhaustedPool(t, cfg)
	ctx := context.Background()

	// A request admitted before the pool ran out, holding the first key's only slot.
	f := newAdmitFixture(t, post, keypool.KeySelectionPrioritized, 10, oneSlotRule())
	adm, err := f.svc.Admit(ctx, f.in, f.acq)
	if err != nil || adm.AcqErr != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
		t.Fatalf("Admit = %+v, %v; want the first key", adm, err)
	}

	svc := NewService(f.svc.snap, keypool.New(shared, discardLogger(), nil, nil), pkgratelimit.New(shared, discardLogger(), nil)).
		WithPostFlight(keypool.New(post, discardLogger(), nil, nil), pkgratelimit.New(post, discardLogger(), nil))
	if _, err := svc.ReserveInbound(ctx, f.in); !errors.Is(err, pkgratelimit.ErrUnavailable) {
		t.Fatalf("reserve on the exhausted shared pool = %v, want ErrUnavailable", err)
	}

	commitsDropped := metrics.KVErrors.WithLabelValues("limit_commit", metrics.KVDropped)
	breakerDropped := metrics.KVErrors.WithLabelValues("breaker_write", metrics.KVDropped)
	commitsBefore, breakerBefore := testutil.ToFloat64(commitsDropped), testutil.ToFloat64(breakerDropped)

	svc.Release(ctx, adm.Acq, keypool.FailureServerError, 0)
	rec, found := keypool.New(post, discardLogger(), nil, nil).ReadCircuit(ctx, f.keys[0].KeyHash)
	if !found || rec.Failures != 1 {
		t.Fatalf("record after Release = %+v (found %v), want one counted failure", rec, found)
	}
	// The refund freed the slot: the next attempt holds it again.
	next, err := f.svc.Acquire(ctx, f.acq)
	if err != nil || next.Key != f.keys[0] {
		t.Fatalf("Acquire after Release = %+v, %v; want the first key's slot back", next, err)
	}

	if err := svc.SettleFailed(ctx, adm.Inbound, next, pkgratelimit.Observations{Cancelled: true}); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	requireSlotFree(t, f)

	if got := testutil.ToFloat64(commitsDropped) - commitsBefore; got != 0 {
		t.Fatalf("limit_commit dropped moved by %v, want 0", got)
	}
	if got := testutil.ToFloat64(breakerDropped) - breakerBefore; got != 0 {
		t.Fatalf("breaker_write dropped moved by %v, want 0", got)
	}
}
