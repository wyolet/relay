package tokencount

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

const (
	outcomeWritten = "written"
	outcomeSkipped = "skipped"
	outcomeFailed  = "failed"
)

// ratioWrites shows how much kv traffic calibration costs: skipped samples were within the threshold of the ratio last written.
var ratioWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "tokencount",
	Name:      "ratio_writes_total",
	Help:      "Calibration samples per ratio key, by outcome: written to kv, skipped (within 5% of the ratio this pod last wrote, written less than 5 minutes ago), or failed (the kv write errored).",
}, []string{"outcome"})

func init() { metrics.Register(ratioWrites) }
