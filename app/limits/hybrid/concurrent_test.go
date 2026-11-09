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
	"github.com/wyolet/relay/sdk/usage"
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

// runHammer admits, releases and settles on the same two counters from many goroutines of one pod, half naming them in the other order, while syncs run every millisecond, and checks that the store ends up holding exactly what was counted.
func runHammer(t *testing.T, store newStore) {
	s := store(t)
	reqs := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, many, time.Hour))
	tokens := callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many, time.Hour))
	firstReqs, firstTokens := bucketAt(t, reqs, time.Now()), bucketAt(t, tokens, time.Now())

	p := New(lua.New(pkgratelimit.New(s, discardLog(), nil)), s, s, time.Millisecond)
	p.Start()
	var admitted, charged atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)
	for g := range 16 {
		cs := []limits.Counter{reqs, tokens}
		if g%2 == 1 {
			cs = []limits.Counter{tokens, reqs}
		}
		wg.Go(func() {
			for i := 0; time.Now().Before(deadline); i++ {
				res, err := p.Admit(context.Background(), cs, limits.Usage{})
				if err != nil {
					t.Error(err)
					return
				}
				if i%7 == 0 {
					res.Release()
					continue
				}
				n := int64(1 + i%5)
				res.Settle(limits.Usage{Tokens: usage.Tokens{"input": n}})
				admitted.Add(1)
				charged.Add(n)
			}
		})
	}
	wg.Wait()
	p.Close()
	t.Logf("admitted %d, settled %d tokens", admitted.Load(), charged.Load())
	if !bucketAt(t, reqs, time.Now()).Equal(firstReqs) || !bucketAt(t, tokens, time.Now()).Equal(firstTokens) {
		t.Skip("a window rolled over during the run")
	}
	if got := stored(t, s, reqs, time.Now()); got != admitted.Load() {
		t.Fatalf("store holds %d requests, admitted %d", got, admitted.Load())
	}
	if got := stored(t, s, tokens, time.Now()); got != charged.Load() {
		t.Fatalf("store holds %d tokens, settled %d", got, charged.Load())
	}
}

func bucketAt(t *testing.T, c limits.Counter, at time.Time) time.Time {
	t.Helper()
	r, err := lua.Rule(&c)
	if err != nil {
		t.Fatal(err)
	}
	start, err := pkgratelimit.BucketStart(r, at)
	if err != nil {
		t.Fatal(err)
	}
	return start
}

func TestHammer_Mem(t *testing.T) { runHammer(t, memStore) }
