package metricslog

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// samples is what one source label holds in a histogram.
type samples struct {
	count uint64
	sum   float64
}

func histogram(t *testing.T, vec *prometheus.HistogramVec, source string) samples {
	t.Helper()
	var m dto.Metric
	if err := vec.WithLabelValues(source).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return samples{m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()}
}

// Buffered requests are recorded by Fill and streamed ones by the stream
// observer, from the same timing marks: every request counts and is timed;
// the overhead split needs the whole upstream leg, admission only its start.
func TestRequestFlowMetrics(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name                         string
		status                       int
		end, upStart, upEnd          time.Duration
		class                        string
		seconds, overhead, admission samples
	}{
		{"reached-upstream", 200, 100 * ms, 5 * ms, 95 * ms, "2xx", samples{1, 0.1}, samples{1, 0.01}, samples{1, 0.005}},
		{"refused-before-upstream", 401, 2 * ms, 0, 0, "4xx", samples{1, 0.002}, samples{}, samples{}},
		{"upstream-never-answered", 502, 50 * ms, 5 * ms, 0, "5xx", samples{1, 0.05}, samples{}, samples{1, 0.005}},
		{"upstream-end-at-its-start", 204, 50 * ms, 5 * ms, 5 * ms, "2xx", samples{1, 0.05}, samples{}, samples{1, 0.005}},
		{"no-status", 0, 10 * ms, 0, 0, "other", samples{1, 0.01}, samples{}, samples{}},
	} {
		for _, path := range []string{"buffered", "streamed"} {
			source := path + "-" + tc.name
			t.Run(source, func(t *testing.T) {
				vecs := map[string]struct {
					vec  *prometheus.HistogramVec
					want samples
				}{
					"request_seconds":   {metrics.RequestSeconds, tc.seconds},
					"overhead_seconds":  {metrics.OverheadSeconds, tc.overhead},
					"admission_seconds": {metrics.AdmissionSeconds, tc.admission},
				}
				total := metrics.RequestsTotal.WithLabelValues(source, tc.class)
				totalBefore, before := testutil.ToFloat64(total), map[string]samples{}
				for name, check := range vecs {
					before[name] = histogram(t, check.vec, source)
				}

				lc := lifecycle.NewContext("req", source, time.Now())
				lc.Timing.End = tc.end
				lc.Timing.Upstream.Start, lc.Timing.Upstream.ResponseEnd = tc.upStart, tc.upEnd

				var got any
				var err error
				if path == "buffered" {
					// The post-flight status is the one recorded, not what the Context carries.
					lc.ResponseStatus = 599
					got, err = New().Fill(lc, &lifecycle.PostFlightEvent{Status: tc.status})
				} else {
					lc.ResponseStatus = tc.status
					obs := New().NewObserver(lc)
					obs.Observe([]byte("data: ignored"))
					got, err = obs.Result()
				}
				if got != nil || err != nil {
					t.Fatalf("result = %v, %v; want nothing stored", got, err)
				}

				if n := testutil.ToFloat64(total) - totalBefore; n != 1 {
					t.Errorf("requests_total{%s} rose by %v, want 1", tc.class, n)
				}
				for name, check := range vecs {
					got := histogram(t, check.vec, source)
					got.count, got.sum = got.count-before[name].count, got.sum-before[name].sum
					if got.count != check.want.count || got.sum < check.want.sum-1e-9 || got.sum > check.want.sum+1e-9 {
						t.Errorf("%s rose by %+v, want %+v", name, got, check.want)
					}
				}
			})
		}
	}
	if name := New().Name(); name != "metrics" {
		t.Fatalf("Name = %q", name)
	}
}

// PreFlight raises the inflight gauge and arms the Metadata note; Collect
// lowers it only when the note is present — a request that never ran
// pre-flight (batch) must not drive the gauge negative.
func TestInflightPairing(t *testing.T) {
	h := New()
	gauge := metrics.InflightRequests.WithLabelValues("pipeline")
	base := testutil.ToFloat64(gauge)
	step := func(what string, want float64) {
		t.Helper()
		if got := testutil.ToFloat64(gauge) - base; got != want {
			t.Fatalf("inflight after %s = %+v, want %+v", what, got, want)
		}
	}

	first := lifecycle.NewContext("req-1", "pipeline", time.Now())
	second := lifecycle.NewContext("req-2", "pipeline", time.Now())
	for _, lc := range []*lifecycle.Context{first, second} {
		if err := h.PreFlight(context.Background(), lc, &lifecycle.PreFlightEvent{}); err != nil {
			t.Fatalf("PreFlight: %v", err)
		}
	}
	step("two admissions", 2)

	h.Collect(first)
	step("one finalize", 1)
	h.Collect(lifecycle.NewContext("req-3", "pipeline", time.Now()))
	h.Collect(nil)
	step("finalizing requests that never ran pre-flight", 1)
	h.Collect(second)
	step("the second finalize", 0)

	// A request with nowhere to carry the note is not counted in, so it can never be counted out.
	bare := &lifecycle.Context{Source: "pipeline"}
	if err := h.PreFlight(context.Background(), bare, nil); err != nil || h.PreFlight(context.Background(), nil, nil) != nil {
		t.Fatalf("PreFlight without metadata: %v", err)
	}
	h.Collect(bare)
	step("a request without metadata", 0)
}
