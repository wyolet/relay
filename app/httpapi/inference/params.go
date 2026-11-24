package inference

import (
	"net/http"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
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
// Each reason adds its own header line, so a request dropping params for two
// reasons reports both.
func surfaceDroppedParams(w http.ResponseWriter, model string, dropped []string, reason string) {
	if len(dropped) == 0 {
		return
	}
	w.Header().Add(httpheader.HeaderWarnings, "dropped_params="+strings.Join(dropped, ",")+"; reason="+reason)
	for _, p := range dropped {
		metrics.DroppedParam(p, model, reason)
	}
}

// hopRenames returns the renames hop's host applies to a body in shape. The binding's own shape reads what the route resolved; another shape (a native or byte-pass-only route) is looked up only when the host has renames at all.
func hopRenames(h *plan.RouteHop, shape adapters.Name) []host.ParamRename {
	if shape == h.Binding.Spec.Adapter {
		return h.ParamRenames
	}
	if len(h.Host.Spec.RequestParamRenames) == 0 {
		return nil
	}
	return h.Host.Spec.ParamRenames(shape)
}

// HopBody is pipeline.Request.HopBody for every inference path: body renamed by hop's host for shape, with the keys dropped because the caller also sent their new name; body itself, untouched and without allocating, when the host renames nothing in shape.
func HopBody(hop *plan.RouteHop, shape adapters.Name, body []byte) ([]byte, []string) {
	renames := hopRenames(hop, shape)
	if len(renames) == 0 {
		return body, nil
	}
	return editHopBody(body, renames, "", nil)
}

// HopBodyFor returns the pipeline.Request.HopBody for a body naming model, sent over hops: HopBody itself when every hop sends model and no hop's binding shape has an envelope in specs, else one that also sets the body's model member to the hop's own UpstreamModel on each hop sending another name, and applies the envelope of the shape the hop is called in. A body without a model member (a shape naming the model in its URL) keeps only the renames and the envelope. Nil specs apply no envelope.
func HopBodyFor(specs *adapter.Registry, hops []plan.RouteHop, model string) func(*plan.RouteHop, adapters.Name, []byte) ([]byte, []string) {
	for i := range hops {
		if hops[i].UpstreamModel != model || requestEnvelope(specs, hops[i].Binding.Spec.Adapter) != nil {
			return func(hop *plan.RouteHop, shape adapters.Name, body []byte) ([]byte, []string) {
				env := requestEnvelope(specs, shape)
				if hop.UpstreamModel == model && env == nil {
					return HopBody(hop, shape, body)
				}
				setModel := ""
				if hop.UpstreamModel != model {
					setModel = hop.UpstreamModel
				}
				return editHopBody(body, hopRenames(hop, shape), setModel, env)
			}
		}
	}
	return HopBody
}

func requestEnvelope(specs *adapter.Registry, shape adapters.Name) *adapter.RequestEnvelope {
	if s := specs.Spec(shape); s != nil {
		return s.Envelope
	}
	return nil
}

// renameMembers returns the edits moving each renamed member present in the object to its new key. When the new key is already set the old one is only removed; those keys are returned too.
func (o *jsonObject) renameMembers(renames []host.ParamRename) ([]memberEdit, []string) {
	if !o.isObject {
		return nil, nil
	}
	var edits []memberEdit
	var superseded []string
	for _, r := range renames {
		var value []byte
		from := editIndex(edits, r.From)
		if from >= 0 {
			value = edits[from].value
		} else if i := o.lastIndex(r.From); i >= 0 {
			value = o.b[o.members[i].valStart:o.members[i].valEnd]
		}
		if value == nil {
			continue
		}
		if from >= 0 {
			edits[from].value = nil
		} else {
			edits = append(edits, memberEdit{key: r.From})
		}
		to := editIndex(edits, r.To)
		switch {
		case to >= 0 && edits[to].value != nil, to < 0 && o.lastIndex(r.To) >= 0:
			superseded = append(superseded, r.From)
		case to >= 0:
			edits[to].value = value
		default:
			edits = append(edits, memberEdit{key: r.To, value: value})
		}
	}
	return edits, superseded
}

// editHopBody applies renames to the top-level members of body, then, when model is set and env keeps the model member, sets the body's top-level model member to it if the body has one, then env's drops and defaults (env nil for none); body itself when nothing changes.
func editHopBody(body []byte, renames []host.ParamRename, model string, env *adapter.RequestEnvelope) ([]byte, []string) {
	var inline [16]jsonMember
	obj, err := scanJSONObject(body, inline[:])
	if err != nil {
		return body, nil
	}
	edits, superseded := obj.renameMembers(renames)
	if model != "" && obj.lastIndex("model") >= 0 && (env == nil || !slices.Contains(env.DropMembers, "model")) {
		edits = append(edits, memberEdit{key: "model", value: appendJSONString(nil, model)})
	}
	if env != nil && obj.isObject {
		edits = obj.envelopeMembers(edits, env)
	}
	if len(edits) == 0 {
		return body, nil
	}
	return obj.assemble(edits), superseded
}

// envelopeMembers appends to edits the removal of each member env drops, then each default env sets for a key the body, as edited, lacks.
func (o *jsonObject) envelopeMembers(edits []memberEdit, env *adapter.RequestEnvelope) []memberEdit {
	for _, key := range env.DropMembers {
		switch at := editIndex(edits, key); {
		case at >= 0:
			edits[at].value = nil
		case o.lastIndex(key) >= 0:
			edits = append(edits, memberEdit{key: key})
		}
	}
	for _, m := range env.DefaultMembers {
		switch at := editIndex(edits, m.Key); {
		case at >= 0 && edits[at].value == nil:
			edits[at].value = m.Value
		case at < 0 && o.lastIndex(m.Key) < 0:
			edits = append(edits, memberEdit{key: m.Key, value: m.Value})
		}
	}
	return edits
}

func appendOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
