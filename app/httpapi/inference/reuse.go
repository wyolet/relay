package inference

// Exports of internal request helpers so the batch subsystem can run an
// item through the realtime pipeline without forking the (fiddly) one-pass
// request parse, the model-field rewrite, or the capability-driven param
// strip. Single source of truth stays here.

import (
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/metrics"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ParseRequest parses a request body once, refusing what the realtime routes
// refuse. Its Model may be empty; the caller decides what that means.
func ParseRequest(body []byte) (*ParsedRequest, error) {
	req, rej := parseRequest(body, v1.RequestKeys{})
	if rej != nil {
		return nil, rej
	}
	return req, nil
}

// UpstreamBody returns req's body with its model set to upstreamModel and
// the params m declares unsupported removed at their paramPaths locations,
// recording the per-param drop metric, so capability gating holds on every
// path to Pipeline.Run. Host renames are per hop: set HopBody on the request.
func UpstreamBody(req *ParsedRequest, upstreamModel string, m *model.Model, paramPaths map[string]string) ([]byte, []string) {
	var edits []memberEdit
	var dropped []string
	if m != nil && len(m.Spec.Capabilities.UnsupportedParams) > 0 {
		edits, dropped = req.stripParams(nil, m.Spec.Capabilities.UnsupportedParams, paramPaths)
		for _, p := range dropped {
			metrics.DroppedParam(p, m.Meta.Name, metrics.DropUnsupportedByModel)
		}
	}
	return req.upstreamBody(upstreamModel, edits), dropped
}

// CountRenameDrops records the drop metric for the keys a hop's renames dropped from the body it was sent (pipeline.Result.BodyDropped).
func CountRenameDrops(modelName string, dropped []string) {
	for _, p := range dropped {
		metrics.DroppedParam(p, modelName, metrics.DropSupersededByRename)
	}
}
