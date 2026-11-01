package inference

import (
	"context"
	"net/http"

	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// resolveRoute looks modelRef up in the caller's route table: one sharded map read, a fill on first use. Reports false after firing the usage failure and writing the refusal.
func (d Deps) resolveRoute(ctx context.Context, w http.ResponseWriter, p *Principal, modelRef string) (*plan.Route, bool) {
	route := p.Caller.Routes.Get(modelRef)
	if route.Refused != nil {
		d.fireUsageFailure(ctx, routeRefusalKind(route.Refused), route.Refused.Error())
		mapRouteRefusal(w, route.Refused, modelRef)
		return nil, false
	}
	return route, true
}

// keepHops returns the hops keep accepts, hops itself when it accepts them all, so the common one-shape route allocates nothing.
func keepHops(hops []plan.RouteHop, keep func(*plan.RouteHop) bool) []plan.RouteHop {
	for i := range hops {
		if keep(&hops[i]) {
			continue
		}
		out := make([]plan.RouteHop, 0, len(hops)-1)
		out = append(out, hops[:i]...)
		for j := i + 1; j < len(hops); j++ {
			if keep(&hops[j]) {
				out = append(out, hops[j])
			}
		}
		return out
	}
	return hops
}

// pipelineRequest is the pipeline input for one dispatched request over hops.
func pipelineRequest(r *http.Request, in DispatchInput, route *plan.Route, hops []plan.RouteHop, a pipeline.Adapter, body []byte, lc *lifecycle.Context) *pipeline.Request {
	return &pipeline.Request{
		Body:          body,
		Headers:       forwardHeaders(r.Header),
		Adapter:       a,
		Model:         route.Model,
		Provider:      route.Provider,
		Hops:          hops,
		ModelName:     route.Model.Meta.Name,
		UpstreamModel: route.UpstreamModel,
		Stream:        in.Stream,
		Lifecycle:     lc,
	}
}

// applyRouteIdentity stamps what the route decided onto lc: the model, the innermost caller policy, and the first hop's host and pricing (the pipeline restamps them per attempt).
func applyRouteIdentity(lc *lifecycle.Context, route *plan.Route, hops []plan.RouteHop, p *Principal) {
	if lc == nil || route == nil {
		return
	}
	if pol := p.Policy(); pol != nil {
		lc.PolicyID, lc.PolicyName = pol.Meta.ID, pol.Meta.Name
	}
	lc.ModelID, lc.ModelName = route.Model.Meta.ID, route.Model.Meta.Name
	if route.Provider != "" {
		lc.ProviderName = route.Provider
	}
	if len(hops) > 0 {
		h := &hops[0]
		lc.HostID, lc.HostName = h.Host.Meta.ID, h.Host.Meta.Name
		if h.Pricing != nil {
			lc.PricingID, lc.PricingName = h.Pricing.Meta.ID, h.Pricing.Meta.Name
		}
	}
	if route.ResolvedVia != "" {
		lc.Metadata["resolved_via"] = route.ResolvedVia
	}
}
