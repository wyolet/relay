package httpapi

import (
	"context"
	"math"
	rtmetrics "runtime/metrics"
	"time"

	"github.com/wyolet/relay/pkg/metrics"
)

// The adaptive limit bounds CPU, which a count of requests can't: a request waiting on its upstream costs no CPU, so a pod holds many more slow requests than fast ones before it saturates. Scheduler latency, how long a runnable goroutine waits for a CPU, rises when the pod is CPU-saturated whatever the upstream latency, so it is the signal.
const (
	adaptiveInterval = 100 * time.Millisecond
	// adaptiveFloor keeps a pod that is saturated by something other than requests (GC, a noisy neighbour) admitting enough traffic to stay useful.
	adaptiveFloor   = 64
	adaptiveMinStep = 16
	// minSchedSamples: a p99 over fewer samples is a window's maximum, and an idle pod's few wakeups say nothing about load.
	minSchedSamples = 100
	// breachWindows: one window's p99 spikes past the target at moderate load on a few cores, so the limit only drops once the breach holds this many windows in a row; until then it holds.
	breachWindows = 3
)

// SchedLatency returns the p99 scheduler latency since its previous call and how many samples it covers.
type SchedLatency func() (p99 time.Duration, samples uint64)

const schedLatencyMetric = "/sched/latencies:seconds"

// RuntimeSchedLatency reads the Go runtime's scheduler latency histogram, counting from the call that creates it.
func RuntimeSchedLatency() SchedLatency {
	sample := []rtmetrics.Sample{{Name: schedLatencyMetric}}
	rtmetrics.Read(sample)
	prev := append([]uint64(nil), sample[0].Value.Float64Histogram().Counts...)
	delta := make([]uint64, len(prev))
	return func() (time.Duration, uint64) {
		rtmetrics.Read(sample)
		h := sample[0].Value.Float64Histogram()
		for i, c := range h.Counts {
			delta[i] = c - prev[i]
		}
		copy(prev, h.Counts)
		return histogramP99(delta, h.Buckets)
	}
}

// histogramP99 returns the upper bound of the bucket holding the 99th percentile of counts (bucket i spans buckets[i] to buckets[i+1]) and the total count.
func histogramP99(counts []uint64, buckets []float64) (time.Duration, uint64) {
	var total uint64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0, 0
	}
	rank := (total*99 + 99) / 100
	var seen uint64
	for i, c := range counts {
		seen += c
		if seen < rank {
			continue
		}
		bound := buckets[i+1]
		if math.IsInf(bound, 1) {
			bound = buckets[i]
		}
		return time.Duration(bound * float64(time.Second)), total
	}
	return 0, total
}

// AdaptLimit adjusts the effective in-flight limit every adaptiveInterval until ctx ends: down while sched reports a p99 above target, back up toward the hard cap while it doesn't.
func (a *Admission) AdaptLimit(ctx context.Context, target time.Duration, sched SchedLatency) {
	tick := time.NewTicker(adaptiveInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p99, samples := sched()
			metrics.SetSchedLatencyP99(p99)
			a.adjustLimit(p99, samples, target)
		}
	}
}

// adjustLimit applies one window: from the breachWindows-th overloaded window in a row it drops the limit to 90 % of what is in flight (or of the limit, when lower) so the pod sheds right away, and holds it before that; healthy raises it by a tenth, at least adaptiveMinStep. Only AdaptLimit calls it, so the load and store and breaches don't race another writer.
func (a *Admission) adjustLimit(p99 time.Duration, samples uint64, target time.Duration) {
	limit, hard := a.limit.Load(), int64(cap(a.sem))
	if samples >= minSchedSamples && p99 > target {
		a.breaches++
		if a.breaches >= breachWindows {
			limit = max(min(adaptiveFloor, hard), min(limit, int64(len(a.sem)))*9/10)
		}
	} else {
		a.breaches = 0
		limit = min(hard, limit+max(limit/10, adaptiveMinStep))
	}
	a.limit.Store(limit)
	metrics.SetInflightAdaptiveLimit(int(limit))
}
