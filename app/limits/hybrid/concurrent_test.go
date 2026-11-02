package hybrid

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// runConcurrent admits from several goroutines on two pods while their sync loops run, across the switch to exact mode, and checks that the store ends up holding exactly what was admitted.
func runConcurrent(t *testing.T, store newStore) {
	s := store(t)
	c := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 2000, time.Hour))
	r, _ := lua.Rule(&c)
	first, _ := pkgratelimit.BucketStart(r, time.Now())

	pods := make([]*Limiter, 2)
	for i := range pods {
		pods[i] = New(lua.New(pkgratelimit.New(s, discardLog(), nil)), s, s, 10*time.Millisecond)
		pods[i].Start()
	}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)
	for g := range 4 {
		wg.Go(func() {
			p := pods[g%len(pods)]
			for time.Now().Before(deadline) {
				res, err := p.Admit(context.Background(), []limits.Counter{c}, limits.Usage{})
				var refused *limits.Refused
				switch {
				case err == nil:
					admitted.Add(1)
					res.Settle(limits.Usage{})
				case errors.As(err, &refused):
				default:
					t.Error(err)
					return
				}
				time.Sleep(50 * time.Microsecond)
			}
		})
	}
	wg.Wait()
	for _, p := range pods {
		p.Close()
	}
	if last, _ := pkgratelimit.BucketStart(r, time.Now()); !last.Equal(first) {
		t.Skip("the window rolled over during the run")
	}
	if got := stored(t, s, c, time.Now()); got != admitted.Load() {
		t.Fatalf("store holds %d, admitted %d", got, admitted.Load())
	}
	if n := admitted.Load(); n > 2000+400 {
		t.Fatalf("admitted %d against a cap of 2000", n)
	}
}

func TestConcurrent_Mem(t *testing.T) { runConcurrent(t, memStore) }
