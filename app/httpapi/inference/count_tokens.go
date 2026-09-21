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
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
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
			RelayKeyAuthMiddleware(d.Catalog),
		).Post("/"+p.Name()+route.TokenCountPath(), func(w http.ResponseWriter, r *http.Request) {
			handleCountTokens(d, w, r.WithContext(clientprofile.WithProfile(r.Context(), profile)))
		})
	}
}

// handleCountTokens answers how many input tokens the posted request would consume. It resolves the same Plan a generation of that body would take, then answers from the best source that plan affords: the upstream's own counter, the ratio relay measured on comparable traffic, or bytes alone.
//
// No usage event and no rate-limit reservation: counting is not a generation, and charging the caller's budget for sizing a prompt would shrink the budget for the prompt itself.
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

	modelName, _, err := extractModelStream(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}
	if modelName == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "field 'model' is required")
		return
	}

	profile := clientprofile.FromContext(ctx)
	if namer, ok := profile.(clientprofile.ModelNamer); ok {
		modelName = namer.Inbound(modelName)
	}

	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	modelRef := modelName
	if uh := r.Header.Get(httpheader.HeaderUpstreamHost); uh != "" && !strings.Contains(modelRef, "@") {
		modelRef = modelRef + "@" + uh
	}
	plan, err := d.Resolver.Resolve(routing.Request{
		ModelName:    modelRef,
		RawModelName: modelName,
		RelayKey:     rk,
	})
	if err != nil {
		mapRoutingErr(w, err, modelRef, rk.Spec.PolicyID)
		return
	}

	if counter, ok := exactCounter(d, profile, plan); ok {
		count, err := countUpstream(d, r, plan, counter, body)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, "upstream_error", "count_tokens_failed", err.Error())
			return
		}
		writeTokenCount(w, count, tierExact)
		return
	}

	if ratio, ok := d.TokenCalibrator.Ratio(ctx, sessionKeyFor(profile, r.Header), plan.Model.Meta.ID); ok {
		writeTokenCount(w, int(math.Round(float64(len(body))*ratio)), tierCalibrated)
		return
	}

	writeTokenCount(w, len(body)/bytesPerEstimatedToken, tierEstimated)
}

// exactCounter returns the upstream's own token counter when this route can use it. It requires the profile's inbound shape to be the upstream's shape too: the counting endpoint is handed the caller's body verbatim, so a body in another vendor's shape would be rejected rather than counted.
func exactCounter(d Deps, profile clientprofile.Profile, plan *routing.Plan) (adapter.TokenCounter, bool) {
	if d.Pipeline == nil || d.Pipeline.Policy == nil {
		return nil, false
	}
	if adapters.Name(profile.Shape()) != plan.HostBinding.Spec.Adapter {
		return nil, false
	}
	counter, ok := d.Specs.PipelineAdapter(plan.HostBinding.Spec.Adapter).(adapter.TokenCounter)
	return counter, ok
}

// countUpstream picks a key the way the pipeline does — healthy per the circuit breaker, chosen by the policy's selection algorithm — and asks the upstream to count. The model field is rewritten to the binding's upstream name, exactly as the byte-pass generation path does.
func countUpstream(d Deps, r *http.Request, plan *routing.Plan, counter adapter.TokenCounter, body []byte) (int, error) {
	ctx := r.Context()
	key, err := d.Pipeline.Policy.PickKey(ctx, plan.Policy, plan.Keys)
	if err != nil {
		return 0, err
	}
	oauth := key.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth
	return counter.CountTokens(ctx, plan.Host.Spec.BaseURL, key.Resolved,
		rewriteModelField(body, plan.UpstreamModel()), forwardHeaders(r.Header), oauth)
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
