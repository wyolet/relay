package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

// failingScripts is a Mem whose scripts all fail with err.
type failingScripts struct {
	*kv.Mem
	err error
}

func (f failingScripts) RunScript(context.Context, string, string, []string, ...any) ([]byte, error) {
	return nil, f.err
}

func TestReserveRefusedWhenStoreUnavailable(t *testing.T) {
	store := failingScripts{Mem: newTestStore(t), err: fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)}
	l := New(store, discardLog(), nil)
	refused := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVRefused)
	before := testutil.ToFloat64(refused)

	res, err := l.Reserve(context.Background(), "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if res != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Reserve = %v, %v; want nil, ErrUnavailable", res, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v lost its cause", err)
	}
	if got := testutil.ToFloat64(refused) - before; got != 1 {
		t.Fatalf("refused counter moved by %v, want 1", got)
	}

	// The reserve queued on an admission batch is refused the same way.
	b := kv.NewBatch(store)
	p, err := l.PrepareReserve(b, "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	b.Exec(context.Background())
	if res, err := p.Finish(context.Background()); res != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("batched reserve = %v, %v; want nil, ErrUnavailable", res, err)
	}
	if got := testutil.ToFloat64(refused) - before; got != 2 {
		t.Fatalf("refused counter moved by %v, want 2", got)
	}
}

// A script the server rejected is a fault in relay, not an outage, so it is not answered as one.
func TestReserveScriptErrorIsNotUnavailable(t *testing.T) {
	store := failingScripts{Mem: newTestStore(t), err: errors.New("ERR user_script:1: bad")}
	l := New(store, discardLog(), nil)

	_, err := l.Reserve(context.Background(), "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want a script error that is not ErrUnavailable", err)
	}
}

func TestCommitFailureCounted(t *testing.T) {
	mem := newTestStore(t)
	l := New(mem, discardLog(), nil)
	res, err := l.Reserve(context.Background(), "policy:p", []Rule{conRule("c", "c", 10, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	dropped := metrics.KVErrors.WithLabelValues("limit_commit", metrics.KVDropped)
	before := testutil.ToFloat64(dropped)

	l.runner = failingScripts{Mem: mem, err: fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)}
	if err := l.Commit(context.Background(), res, Observations{}); err == nil {
		t.Fatal("Commit succeeded against a failing store")
	}
	if got := testutil.ToFloat64(dropped) - before; got != 1 {
		t.Fatalf("dropped counter moved by %v, want 1", got)
	}
}
