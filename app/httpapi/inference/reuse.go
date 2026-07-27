package inference

// Exports of internal request helpers so the batch subsystem can run an
// item through the realtime pipeline without forking the (fiddly) model-field
// rewrite, the minimal model/stream parse, or the capability-driven param
// strip. Single source of truth stays here.

import (
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/metrics"
)

// ExtractModelStream parses the caller-supplied model name and stream flag from
// a request body. Reused by the batch subsystem's per-item runner.
func ExtractModelStream(body []byte) (model string, stream bool, err error) {
	return extractModelStream(body)
}

// RewriteModelField returns body with its "model" field rewritten to
// upstreamModel (the resolved upstream model name). Reused by the batch
// subsystem's per-item runner.
func RewriteModelField(body []byte, upstreamModel string) []byte {
	return rewriteModelField(body, upstreamModel)
}

// StripUnsupportedParams applies the routed model's UnsupportedParams to a
// vendor-shaped body via the spec's ParamPaths, recording the per-param drop
// metric. Reused by runners that build pipeline requests outside Dispatch
// (batch) so capability gating holds on every path to Pipeline.Run.
func StripUnsupportedParams(body []byte, m *model.Model, paramPaths map[string]string) ([]byte, []string) {
	if m == nil {
		return body, nil
	}
	u := m.Spec.Capabilities.UnsupportedParams
	if len(u) == 0 {
		return body, nil
	}
	out, dropped := stripWireParams(body, u, paramPaths)
	for _, p := range dropped {
		metrics.DroppedParam(p, m.Meta.Name)
	}
	return out, dropped
}
