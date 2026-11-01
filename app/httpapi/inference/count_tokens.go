package inference

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// How an answer was reached, reported in httpheader.HeaderTokenCount.
const (
	tierExact      = "exact"
	tierCalibrated = "calibrated"
	tierEstimated  = "estimated"
)

// bytesPerEstimatedToken is the divisor clients use when they count characters themselves. Relay answers with the same figure when it knows nothing better, so the endpoint is never worse than the fallback it replaces.
const bytesPerEstimatedToken = 4

// mountTokenCountRoutes gives every profile that declares one a POST count-tokens endpoint under its /{profile} prefix, on the same chain as /v1/* (readiness → classify → relay-key auth) since the answer depends on what the caller's key may route to.
func mountTokenCountRoutes(r chi.Router, d Deps) {
	for _, p := range d.Profiles.Profiles() {
		route, ok := p.(clientprofile.TokenCountRoute)
		if !ok {
			continue
		}
		profile := p
		r.With(
			ReadinessMiddleware(d.Catalog),
			ClassifyMiddleware(),
			PrincipalMiddleware(d.Auth()),
		).Post("/"+p.Name()+route.TokenCountPath(), func(w http.ResponseWriter, r *http.Request) {
			handleCountTokens(d, w, withRouteProfile(r, profile))
		})
	}
}

// handleCountTokens answers how many input tokens the posted request would consume. It resolves the same route a generation of that body would take, then answers from the best source that route affords: the upstream's own counter, the ratio relay measured on comparable traffic, or bytes alone.
//
// An upstream count spends an operator credential, so it runs the generation path's admission and pipeline: in-flight cap, the caller's and the credential's counters (one request, no tokens), breakers and failover, and a usage event under its own source. A local answer spends nothing upstream and holds nothing; token revocation was checked at authentication.
func handleCountTokens(d Deps, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if httpmw.IsBodyTooLargeError(err) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	req, rej := parseRequest(body, v1.RequestKeys{})
	if rej != nil {
		writeBodyError(w, rej)
		return
	}
	modelName := req.Model
	if modelName == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "field 'model' is required")
		return
	}

	profile := clientprofile.FromContext(ctx)
	if namer, ok := profile.(clientprofile.ModelNamer); ok {
		modelName = namer.Inbound(modelName)
	}

	principal := PrincipalFrom(ctx)
	if principal == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	modelRef := modelName
	if uh := r.Header.Get(httpheader.HeaderUpstreamHost); uh != "" && !strings.Contains(modelRef, "@") {
		modelRef = modelRef + "@" + uh
	}
	route := principal.Caller.Routes.Get(modelRef)
	if route.Refused != nil {
		mapRouteRefusal(w, route.Refused, modelRef)
		return
	}

	if counter, hops, ok := exactCounter(d, profile, route); ok {
		if count, ok := countUpstream(d, w, r, route, hops, counter, req, modelName); ok {
			writeTokenCount(w, count, tierExact)
		}
		return
	}

	if ratio, ok := d.TokenCalibrator.Ratio(ctx, sessionKeyFor(profile, r.Header), route.Model.Meta.ID); ok {
		writeTokenCount(w, int(math.Round(float64(len(body))*ratio)), tierCalibrated)
		return
	}

	writeTokenCount(w, len(body)/bytesPerEstimatedToken, tierEstimated)
}

// exactCounter returns the upstream's own token counter when this route can use it, with the hops that speak the first hop's shape. It requires the profile's inbound shape to be the upstream's shape too: the counting endpoint is handed the caller's body verbatim, so a body in another vendor's shape would be rejected rather than counted.
func exactCounter(d Deps, profile clientprofile.Profile, route *plan.Route) (adapter.TokenCounter, []plan.RouteHop, bool) {
	if d.Pipeline == nil || d.Pipeline.Limiter == nil {
		return nil, nil, false
	}
	shape := route.Hops[0].Binding.Spec.Adapter
	if !clientprofile.Speaks(profile, string(shape)) {
		return nil, nil, false
	}
	counter, ok := d.Specs.PipelineAdapter(shape).(adapter.TokenCounter)
	if !ok {
		return nil, nil, false
	}
	return counter, keepHops(route.Hops, func(h *plan.RouteHop) bool { return h.Binding.Spec.Adapter == shape }), true
}

// countUpstream asks the upstream to count through the admission step and pipeline a generation takes. The model field is rewritten to the binding's upstream name, exactly as the byte-pass generation path does. Reports false after writing the error.
func countUpstream(d Deps, w http.ResponseWriter, r *http.Request, route *plan.Route, hops []plan.RouteHop, counter adapter.TokenCounter, req *ParsedRequest, requested string) (int, bool) {
	ctx := r.Context()
	lc := mintLifecycle(ctx, sourceCount, ClassificationFrom(ctx).ClientIP)
	lc.RequestedModel = requested
	applyObsHeaders(lc, r.Header, d.TrustEventTime)
	applyProfile(lc, clientprofile.FromContext(ctx), r.Header)
	applyRouteIdentity(lc, route, hops, PrincipalFrom(ctx))
	ctx = lifecycle.ContextWith(ctx, lc)
	inbound := d.Specs.Spec(adapters.Name(clientprofile.FromContext(ctx).Shape()))
	if !d.runPreFlight(ctx, w, lc, inbound) {
		return 0, false
	}

	result, err := d.Pipeline.Run(ctx, &pipeline.Request{
		Body:          req.upstreamBody(route.UpstreamModel, nil),
		Headers:       forwardHeaders(r.Header),
		Adapter:       counter.CountAdapter(),
		Model:         route.Model,
		Provider:      route.Provider,
		Hops:          hops,
		ModelName:     route.Model.Meta.Name,
		UpstreamModel: route.UpstreamModel,
		Lifecycle:     lc,
	})
	if err != nil {
		mapRunErr(w, inbound, err)
		return 0, false
	}
	raw, err := io.ReadAll(io.LimitReader(result.Body, adapter.MaxCountBody))
	_ = result.Body.Close()
	if err == nil {
		var count int
		if count, err = adapter.ParseTokenCount(result.Status, raw); err == nil {
			return count, true
		}
	}
	writeAPIError(w, http.StatusBadGateway, "upstream_error", "count_tokens_failed", err.Error())
	return 0, false
}

// sessionKeyFor asks the profile which conversation this request belongs to. Empty for a profile that marks no sessions — the calibrator then falls back to the model's ratio.
func sessionKeyFor(p clientprofile.Profile, h http.Header) string {
	keyer, ok := p.(clientprofile.SessionKeyer)
	if !ok {
		return ""
	}
	return keyer.SessionKey(h)
}

// writeTokenCount emits the client's expected body and names the tier that produced it.
func writeTokenCount(w http.ResponseWriter, count int, tier string) {
	if count < 0 {
		count = 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(httpheader.HeaderTokenCount, tier)
	w.WriteHeader(http.StatusOK)
	body, _ := json.Marshal(struct {
		InputTokens int `json:"input_tokens"`
	}{InputTokens: count})
	_, _ = w.Write(body)
}
