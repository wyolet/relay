package inference

import (
	"bytes"
	"encoding/json"
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

// stripWireParams removes unsupported params from a vendor-shaped request
// body on the byte-pass path, using the spec's param→JSON-path map. Same
// technique as rewriteModelField: RawMessage maps keep every untouched value
// byte-identical, and the body is only re-encoded when something was
// actually removed — the common (no params present) case returns the
// original slice.
func stripWireParams(body []byte, unsupported []string, paths map[string]string) ([]byte, []string) {
	if len(paths) == 0 {
		return body, nil
	}
	var fields map[string]json.RawMessage
	var dropped []string
	for _, p := range unsupported {
		path, ok := paths[p]
		if !ok {
			continue
		}
		if fields == nil {
			if err := json.Unmarshal(body, &fields); err != nil {
				return body, nil
			}
		}
		if deleteAtPath(fields, strings.Split(path, ".")) {
			dropped = append(dropped, p)
		}
	}
	if len(dropped) == 0 {
		return body, nil
	}
	out, err := encodeJSONObject(fields, len(body))
	if err != nil {
		return body, nil
	}
	return out, dropped
}

// deleteAtPath removes the leaf key addressed by segs, re-encoding only the
// sub-objects along the path. Reports whether the key existed.
func deleteAtPath(fields map[string]json.RawMessage, segs []string) bool {
	if len(segs) == 1 {
		if _, ok := fields[segs[0]]; !ok {
			return false
		}
		delete(fields, segs[0])
		return true
	}
	raw, ok := fields[segs[0]]
	if !ok {
		return false
	}
	var sub map[string]json.RawMessage
	if json.Unmarshal(raw, &sub) != nil {
		return false
	}
	if !deleteAtPath(sub, segs[1:]) {
		return false
	}
	nb, err := encodeJSONObject(sub, len(raw))
	if err != nil {
		return false
	}
	fields[segs[0]] = nb
	return true
}

func encodeJSONObject(fields map[string]json.RawMessage, sizeHint int) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(sizeHint)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
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
