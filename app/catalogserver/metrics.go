package catalogserver

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type metrics struct {
	registry        *prometheus.Registry
	lastFetch       prometheus.Gauge
	fetchFailures   prometheus.Counter
	builds          *prometheus.CounterVec
	buildFailures   *prometheus.CounterVec
	mismatches      *prometheus.CounterVec
	releaseFailures *prometheus.CounterVec
	registrations   *prometheus.CounterVec
	nudges          *prometheus.CounterVec
	githubHooks     prometheus.Counter
}

// newMetrics uses its own registry, so a Server never shares series with
// relay's metrics or with another Server in the same process.
func newMetrics(s *Server) *metrics {
	const ns = "catalog_server"
	m := &metrics{
		registry: prometheus.NewRegistry(),
		lastFetch: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "last_fetch_success_timestamp_seconds",
			Help: "Unix time of the last successful git fetch.",
		}),
		fetchFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "fetch_failures_total",
			Help: "Git fetches that failed.",
		}),
		builds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "builds_total",
			Help: "Release files rebuilt from the mirror, by asset (catalog, data).",
		}, []string{"asset"}),
		buildFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "build_failures_total",
			Help: "Rebuilds that failed, e.g. a schema this server can't compose, by asset.",
		}, []string{"asset"}),
		mismatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "rebuild_mismatch_total",
			Help: "Rebuilt files whose sha256 differs from the published sum, by asset. The published file is served instead.",
		}, []string{"asset"}),
		releaseFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "release_fetch_failures_total",
			Help: "Published sums or files that could not be fetched or failed their sum, by asset.",
		}, []string{"asset"}),
		registrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "webhook_registrations_total",
			Help: "Receiver registrations by result (created, renewed, invalid, refused, challenge_failed, rate_limited, full).",
		}, []string{"result"}),
		nudges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "webhook_nudges_total",
			Help: "Release nudge sends by result: delivered or failed per attempt, gave_up once a receiver's retries for a tag ran out.",
		}, []string{"result"}),
		githubHooks: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "github_hooks_total",
			Help: "Repository push hooks received; each asks for an early sync.",
		}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.lastFetch, m.fetchFailures, m.builds, m.buildFailures, m.mismatches, m.releaseFailures,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "tags_known",
			Help: "Release tags in the mirror.",
		}, func() float64 { return float64(s.countTags(false)) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "tags_ready",
			Help: "Release tags whose two files are published and verified.",
		}, func() float64 { return float64(s.countTags(true)) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "cache_bytes",
			Help: "Bytes of release files held in memory, pinned channel-latest files included.",
		}, func() float64 { return float64(s.cache.bytes()) }),
	)
	if s.hooks != nil {
		m.registry.MustRegister(m.registrations, m.nudges, m.githubHooks, subscriptionsCollector{
			desc: prometheus.NewDesc(prometheus.BuildFQName(ns, "", "webhook_subscriptions"),
				"Live receiver subscriptions held by this replica, by channel.", []string{"channel"}, nil),
			subs: s.hooks.subs,
			now:  func() time.Time { return s.now() },
		})
	}
	return m
}

func (s *Server) countTags(verified bool) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !verified {
		return len(s.tags)
	}
	n := 0
	for _, st := range s.tags {
		if st.verified[assetCatalog] != nil && st.verified[assetData] != nil {
			n++
		}
	}
	return n
}
