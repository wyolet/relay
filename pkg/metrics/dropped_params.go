package metrics

import "github.com/prometheus/client_golang/prometheus"

// DroppedParams counts request parameters the relay stripped before the
// upstream call because the routed model declares them unsupported
// (Capabilities.UnsupportedParams). A sustained rate names the models whose
// callers still send sampling params the upstream would reject — the strip
// is deliberate, but it should be visible, never silent.
var DroppedParams = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "dropped_params_total",
		Help:      "Request parameters stripped pre-upstream because the routed model declares them unsupported, by param and model.",
	},
	[]string{"param", "model"},
)

func init() { Register(DroppedParams) }

// DroppedParam records one stripped parameter for one request.
func DroppedParam(param, model string) {
	DroppedParams.WithLabelValues(param, model).Inc()
}
