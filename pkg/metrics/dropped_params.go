package metrics

import "github.com/prometheus/client_golang/prometheus"

// Reasons a request parameter is dropped before the upstream call; the same
// strings name the reason in the response warning header.
const (
	// DropUnsupportedByModel: the routed model declares the param unsupported.
	DropUnsupportedByModel = "unsupported_by_model"
	// DropSupersededByRename: the host renames the param to a key the caller
	// also sent, so the caller's value under the new key wins.
	DropSupersededByRename = "superseded_by_rename"
)

// DroppedParams counts request parameters the relay stripped before the
// upstream call: params the routed model declares unsupported
// (Capabilities.UnsupportedParams), and params a host rename makes redundant.
// A sustained rate names the models whose callers still send params the
// upstream would reject — the strip is deliberate, but it should be visible,
// never silent.
var DroppedParams = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "dropped_params_total",
		Help:      "Request parameters stripped pre-upstream, by param, model and reason (unsupported_by_model, superseded_by_rename).",
	},
	[]string{"param", "model", "reason"},
)

func init() { Register(DroppedParams) }

// DroppedParam records one stripped parameter for one request.
func DroppedParam(param, model, reason string) {
	DroppedParams.WithLabelValues(param, model, reason).Inc()
}
