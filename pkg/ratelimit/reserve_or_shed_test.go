package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

// busyPool is a Mem whose connection wait queue is always full.
type busyPool struct{ *kv.Mem }

func (busyPool) ExecBatchOrShed(_ context.Context, ops []kv.BatchOp) {
	for _, op := range ops {
		op.Result.Err = kv.ErrPoolBusy
	}
}

func (busyPool) RunScriptOrShed(context.Context, string, string, []string, ...any) ([]byte, error) {
	return nil, kv.ErrPoolBusy
}

func (busyPool) PoolBusy() bool { return true }

// runReserveOrShedMatchesReserve pins that on a store with room ReserveOrShed admits, counts and refuses exactly as Reserve does.
func runReserveOrShedMatchesReserve(t *testing.T, l *Limiter) {
	t.Helper()
	ctx := context.Background()
	rules := []Rule{reqRule("ros", "ros", 2, time.Minute)}
	for i := range 2 {
		res, err := l.ReserveOrShed(ctx, "policy:ros", rules)
		if err != nil || res == nil {
			t.Fatalf("reserve %d = %v, %v; want admitted", i, res, err)
		}
	}
	var exceeded *ExceededError
	if _, err := l.Reserve(ctx, "policy:ros", rules); !errors.As(err, &exceeded) {
		t.Fatalf("Reserve after two ReserveOrShed = %v, want ExceededError: both share the bucket", err)
	}
	if _, err := l.ReserveOrShed(ctx, "policy:ros", rules); !errors.As(err, &exceeded) {
		t.Fatalf("ReserveOrShed over the limit = %v, want ExceededError", err)
	}
}

func TestReserveOrShedMatchesReserveMem(t *testing.T) {
	runReserveOrShedMatchesReserve(t, New(newTestStore(t), discardLog(), nil))
}

// A full wait queue refuses the reserve as unavailable, counted as shed rather than as a refusal after the wait.
func TestReserveOrShedRefusedWhilePoolBusy(t *testing.T) {
	mem := newTestStore(t)
	RegisterScripts(mem)
	l := New(busyPool{Mem: mem}, discardLog(), nil)
	shed := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVShed)
	refused := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVRefused)
	shedBefore, refusedBefore := testutil.ToFloat64(shed), testutil.ToFloat64(refused)

	res, err := l.ReserveOrShed(context.Background(), "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if res != nil || !errors.Is(err, ErrUnavailable) || !errors.Is(err, kv.ErrPoolBusy) {
		t.Fatalf("ReserveOrShed = %v, %v; want nil, ErrUnavailable from ErrPoolBusy", res, err)
	}
	if got := testutil.ToFloat64(shed) - shedBefore; got != 1 {
		t.Fatalf("shed counter moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(refused) - refusedBefore; got != 0 {
		t.Fatalf("refused counter moved by %v, want 0", got)
	}

	// Reserve never sheds: it queues for the store as before.
	if _, err := l.Reserve(context.Background(), "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)}); err != nil {
		t.Fatalf("Reserve on a busy pool = %v, want admitted", err)
	}
}
