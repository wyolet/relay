package lua

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// PrepareAdmit reserves every group in one round trip and, like Admit, leaves nothing held when a group refuses.
func TestPrepareAdmit(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	rec := kvtest.NewRecording(memStore(t))
	l := newLimiter(rec, &now)
	lease := callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "project:x")
	up := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "a")

	prepareAdmit := func(cs ...limits.Counter) (limits.Reservation, error) {
		b := kv.NewBatch(rec, 2)
		p, err := l.PrepareAdmit(b, cs)
		if err != nil {
			t.Fatal(err)
		}
		b.Exec(context.Background())
		return p.Finish(context.Background())
	}

	trips := rec.RoundTrips()
	res, err := prepareAdmit(lease, up)
	if err != nil {
		t.Fatalf("PrepareAdmit: %v", err)
	}
	if n := rec.RoundTrips() - trips; n != 1 {
		t.Fatalf("two groups took %d round trips, want 1", n)
	}
	res.Settle(limits.Usage{})

	res, err = prepareAdmit(lease, up)
	var r *limits.Refused
	if !errors.As(err, &r) || res != nil || r.Counter.ID != up.ID {
		t.Fatalf("PrepareAdmit = %v, %v; want the account counter's refusal", res, err)
	}
	// The caller group reserved in the same batch; its lease must be free again.
	admit(t, l, lease).Release()
}
