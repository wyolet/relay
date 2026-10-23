package inference

import (
	"net/http"
	"strings"

	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/metrics"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// stripCanonicalParams removes sampling params the routed model declares
// unsupported from every ModelOpts entry of a canonical request. Returns the
// names actually removed (present and non-nil). Unknown names are inert so
// newer catalog data doesn't break older relays.
func stripCanonicalParams(req *v1.Request, unsupported []string) []string {
	var dropped []string
	for _, opts := range req.ModelConfig {
		if opts == nil || opts.Sampling == nil {
			continue
		}
		s := opts.Sampling
		for _, p := range unsupported {
			switch p {
			case "temperature":
				if s.Temperature != nil {
					s.Temperature = nil
					dropped = appendOnce(dropped, p)
				}
			case "top_p":
				if s.TopP != nil {
					s.TopP = nil
					dropped = appendOnce(dropped, p)
				}
			case "top_k":
				if s.TopK != nil {
					s.TopK = nil
					dropped = appendOnce(dropped, p)
				}
			}
		}
	}
	return dropped
}

// surfaceDroppedParams makes a strip visible: response warning header +
// per-param metrics. Deliberately loud-but-non-fatal — see codebase rule 11
// (no silent drops); the model-portability contract is that the request
// still succeeds, the surface is how the caller finds out it was adjusted.
func surfaceDroppedParams(w http.ResponseWriter, model string, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	w.Header().Set(httpheader.HeaderWarnings,
		"dropped_params="+strings.Join(dropped, ",")+"; reason=unsupported_by_model")
	for _, p := range dropped {
		metrics.DroppedParam(p, model)
	}
}

func appendOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
