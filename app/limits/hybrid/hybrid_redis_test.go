//go:build integration

package hybrid

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func redisStore(t testing.TB) kvtest.ScriptStore { return kvtest.NewRedis(t) }

func TestHybrid_Redis(t *testing.T) { runSuite(t, redisStore) }

func TestConcurrent_Redis(t *testing.T) { runConcurrent(t, redisStore) }

func BenchmarkAdmitSettle_Redis(b *testing.B) {
	b.Run("local/lua", func(b *testing.B) { benchmarkAdmitSettle(b, redisStore(b), localSet(), true) })
	b.Run("local/hybrid", func(b *testing.B) { benchmarkAdmitSettle(b, redisStore(b), localSet(), false) })
	b.Run("mixed/lua", func(b *testing.B) { benchmarkAdmitSettle(b, redisStore(b), mixedSet(), true) })
	b.Run("mixed/hybrid", func(b *testing.B) { benchmarkAdmitSettle(b, redisStore(b), mixedSet(), false) })
}

// BenchmarkPods_Redis admits and settles from many goroutines on three limiters, each with a store client of its own as three processes would have, their sync loops running. It reports admit latency percentiles and store round trips per request, syncs included.
func BenchmarkPods_Redis(b *testing.B) {
	for _, kind := range []string{"lua", "hybrid"} {
		for _, par := range []int{1, 16} {
			b.Run(kind+"/goroutines-per-cpu-"+strconv.Itoa(par), func(b *testing.B) { benchmarkPods(b, kind, par) })
		}
	}
}

func benchmarkPods(b *testing.B, kind string, parallelism int) {
	cfg := kvtest.Config(b)
	const pods = 3
	lims := make([]limits.Limiter, pods)
	recs := make([]*kvtest.Recording, pods)
	for i := range lims {
		recs[i] = kvtest.NewRecording(kvtest.Connect(b, cfg))
		exact := lua.New(pkgratelimit.New(recs[i], discardLog(), nil))
		lims[i] = exact
		if kind == "hybrid" {
			h := New(exact, recs[i], recs[i], time.Second)
			h.Start()
			b.Cleanup(h.Close)
			lims[i] = h
		}
	}
	cs := localSet()
	// A pod counts a new counter exactly until its first sync; warm up past that so the run measures the steady state.
	for _, lim := range lims {
		res, err := lim.Admit(context.Background(), cs, limits.Usage{})
		if err != nil {
			b.Fatal(err)
		}
		res.Settle(benchUsage)
		if h, ok := lim.(*Limiter); ok {
			h.sync()
		}
	}
	before := 0
	for _, r := range recs {
		before += r.RoundTrips()
	}
	var (
		next atomic.Int64
		mu   sync.Mutex
		lat  []time.Duration
	)
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		lim := lims[next.Add(1)%pods]
		var mine []time.Duration
		for pb.Next() {
			start := time.Now()
			res, err := lim.Admit(context.Background(), cs, limits.Usage{})
			mine = append(mine, time.Since(start))
			if err != nil {
				b.Error(err)
				return
			}
			res.Settle(benchUsage)
		}
		mu.Lock()
		lat = append(lat, mine...)
		mu.Unlock()
	})
	b.StopTimer()
	slices.Sort(lat)
	if len(lat) > 0 {
		b.ReportMetric(float64(lat[len(lat)/2]), "p50-ns")
		b.ReportMetric(float64(lat[len(lat)*99/100]), "p99-ns")
	}
	trips := -before
	for _, r := range recs {
		trips += r.RoundTrips()
	}
	b.ReportMetric(float64(trips)/float64(b.N), "trips/op")
}
