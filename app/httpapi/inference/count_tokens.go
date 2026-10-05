package inference

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
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
			PrincipalMiddleware(d.Catalog, d.Tokens),
		).Post("/"+p.Name()+route.TokenCountPath(), func(w http.ResponseWriter, r *http.Request) {
			handleCountTokens(d, w, r.WithContext(clientprofile.WithProfile(r.Context(), profile)))
		})
	}
}

// handleCountTokens answers how many input tokens the posted request would consume. It resolves the same Plan a generation of that body would take, then answers from the best source that plan affords: the upstream's own counter, the ratio relay measured on comparable traffic, or bytes alone.
//
// An upstream count spends an operator credential, so it runs the generation path's admission and pipeline: in-flight cap, policy reservation (one request, no tokens), token revocation, key tier and failover, and a usage event under its own source. A local answer spends nothing upstream and only checks token revocation.
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

	principal := PrincipalFrom(ctx)
	if principal == nil {
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
		Policy:       principal.Policy,
		UserID:       principal.UserID,
		Snapshot:     SnapshotFrom(ctx),
	})
	if err != nil {
		mapRoutingErr(w, err, modelRef, principal.PolicyID())
		return
	}

	if counter, ok := exactCounter(d, profile, plan); ok {
		if count, ok := countUpstream(d, w, r, plan, counter, body, modelName); ok {
			writeTokenCount(w, count, tierExact)
		}
		return
	}

	if !refuseRevokedToken(ctx, d, w) {
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
	if !clientprofile.Speaks(profile, string(plan.HostBinding.Spec.Adapter)) {
		return nil, false
	}
	counter, ok := d.Specs.PipelineAdapter(plan.HostBinding.Spec.Adapter).(adapter.TokenCounter)
	return counter, ok
}

// countUpstream asks the upstream to count through the admission step and pipeline a generation takes. The model field is rewritten to the binding's upstream name, exactly as the byte-pass generation path does. Reports false after writing the error.
func countUpstream(d Deps, w http.ResponseWriter, r *http.Request, plan *routing.Plan, counter adapter.TokenCounter, body []byte, requested string) (int, bool) {
	ctx := r.Context()
	lc := mintLifecycle(ctx, d.Catalog, sourceCount, ClassificationFrom(ctx).ClientIP)
	lc.RequestedModel = requested
	applyObsHeaders(lc, r.Header, d.TrustEventTime)
	applyProfile(lc, clientprofile.FromContext(ctx), r.Header)
	applyPlanIdentity(lc, plan)
	ctx = lifecycle.ContextWith(ctx, lc)
	if !d.runPreFlight(ctx, w, lc) {
		return 0, false
	}

	teamID, tokenJTI := reserveIdentity(ctx)
	result, err := d.Pipeline.Run(ctx, &pipeline.Request{
		Body:          rewriteModelField(body, plan.UpstreamModel()),
		Headers:       forwardHeaders(r.Header),
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       counter.CountAdapter(),
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.UpstreamModel(),
		TeamID:        teamID,
		TokenJTI:      tokenJTI,
		Lifecycle:     lc,
	})
	if err != nil {
		mapPipelineErr(w, err)
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

// refuseRevokedToken checks a token's jti against the denylist for a count answered locally, which never reaches the pipeline's reservation. With no policy it meters nothing, so it is one kv read and no commit; a key has no jti and costs nothing. Reports false after writing the response.
func refuseRevokedToken(ctx context.Context, d Deps, w http.ResponseWriter) bool {
	teamID, jti := reserveIdentity(ctx)
	if jti == "" || d.Pipeline == nil || d.Pipeline.Policy == nil {
		return true
	}
	if _, err := d.Pipeline.Policy.ReserveInbound(ctx, policy.InboundInput{TeamID: teamID, TokenJTI: jti}); err != nil {
		mapPipelineErr(w, err)
		return false
	}
	return true
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
