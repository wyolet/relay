package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// Runner executes one batch item through the realtime inference pipeline,
// buffering the response. It deliberately reuses app/pipeline.Pipeline.Run so a
// batch item inherits keypool selection, circuit breakers, failover, and usage
// emission for free — the only difference from a live request is that the
// lifecycle source is "batch" and the response is buffered rather than streamed.
type Runner struct {
	Resolver *routing.Resolver
	Pipeline *pipeline.Pipeline
	Specs    *adapter.Registry
	Catalog  *appcatalog.Catalog
}

// ErrCrossShape is returned when a batch item's inbound shape differs from the
// resolved upstream adapter. v1 batch supports same-shape / byte-pass / native
// only; cross-shape translation is a follow-up. Surfaced explicitly rather than
// silently mis-dispatched (canonical rule 11).
var ErrCrossShape = errors.New("batch: cross-shape dispatch not yet supported")

// Run executes one item. requestID ties the usage event to the item (the jobq
// job id); relayKeyHash + inbound select routing, and attr is the submission's
// attribution as recorded at submit. It returns the upstream status and the
// buffered response body. Usage emits automatically with source="batch" when
// the pipeline body closes.
func (rn *Runner) Run(ctx context.Context, requestID, relayKeyHash string, attr Attribution, inbound adapters.Name, body []byte) (int, []byte, error) {
	snap := rn.Catalog.Current()
	rk, _ := snap.KeyByHash(relayKeyHash)
	if rk == nil {
		return 0, nil, errors.New("batch: relay key not found (revoked or deleted)")
	}

	modelName, _, err := inference.ExtractModelStream(body)
	if err != nil {
		return 0, nil, fmt.Errorf("batch: parse model: %w", err)
	}

	plan, err := rn.Resolver.Resolve(routing.Request{ModelName: modelName, RawModelName: modelName, Key: rk})
	if err != nil {
		return 0, nil, fmt.Errorf("batch: route %q: %w", modelName, err)
	}

	inboundSpec := rn.Specs.Spec(inbound)
	upstreamSpec := rn.Specs.Spec(plan.HostBinding.Spec.Adapter)
	upstreamAdapter := rn.Specs.PipelineAdapter(plan.HostBinding.Spec.Adapter)
	if inboundSpec == nil || upstreamSpec == nil || upstreamAdapter == nil {
		return 0, nil, fmt.Errorf("batch: adapter not registered for %s/%s", inbound, plan.HostBinding.Spec.Adapter)
	}

	sameShape := inbound == plan.HostBinding.Spec.Adapter ||
		inboundSpec.BytePass ||
		(inboundSpec.IsNativePath != nil && inboundSpec.IsNativePath(plan))
	if !sameShape {
		return 0, nil, fmt.Errorf("%w (%s→%s)", ErrCrossShape, inbound, plan.HostBinding.Spec.Adapter)
	}

	lc := lifecycle.NewContext(requestID, "batch", time.Now())
	lc.RelayKeyHash = relayKeyHash
	lc.RequestedModel = modelName
	lc.ProjectID = attr.ProjectID
	lc.TeamID = attr.TeamID
	lc.PrincipalKind = attr.PrincipalKind
	lc.PrincipalID = attr.PrincipalID
	lc.CredentialKind = attr.CredentialKind
	lc.CredentialID = attr.CredentialID
	// Slugs resolve from the snapshot here, as the policy name already does:
	// the ids were fixed at submit, the names describe them as of execution.
	if proj, ok := snap.Project(attr.ProjectID); ok {
		lc.ProjectName = proj.Meta.Name
	}
	if t, ok := snap.Team(attr.TeamID); ok {
		lc.TeamName = t.Meta.Name
	}
	if attr.PrincipalKind == string(key.PrincipalServiceAccount) {
		if sa, ok := snap.ServiceAccount(attr.PrincipalID); ok {
			lc.PrincipalName = sa.Meta.Name
		}
	}
	if plan.Policy != nil {
		lc.PolicyID = plan.Policy.Meta.ID
		lc.PolicyName = plan.Policy.Meta.Name
	}
	if plan.Model != nil {
		lc.ModelID = plan.Model.Meta.ID
		lc.ModelName = plan.Model.Meta.Name
	}
	if plan.Host != nil {
		lc.HostID = plan.Host.Meta.ID
		lc.HostName = plan.Host.Meta.Name
	}
	if plan.Provider != "" {
		lc.ProviderName = plan.Provider
	}
	if plan.Pricing != nil {
		lc.PricingID = plan.Pricing.Meta.ID
		lc.PricingName = plan.Pricing.Meta.Name
	}
	if plan.ResolvedVia != "" {
		lc.Metadata["resolved_via"] = plan.ResolvedVia
	}
	lc.Translator = upstreamSpec.Translator

	wireBody := inference.RewriteModelField(body, plan.UpstreamModel())
	// Same-shape only (guarded above), so the body stays inbound-shaped and
	// the inbound spec's ParamPaths address it — mirrors runBytePass.
	wireBody, _ = inference.StripUnsupportedParams(wireBody, plan.Model, inboundSpec.ParamPaths)

	preq := &pipeline.Request{
		Body:          wireBody,
		Headers:       http.Header{},
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       upstreamAdapter,
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.UpstreamModel(),
		Stream:        false,
		Lifecycle:     lc,
	}

	result, err := rn.Pipeline.Run(ctx, preq)
	if err != nil {
		return 0, nil, err
	}
	defer result.Body.Close()
	out, rerr := io.ReadAll(result.Body)
	if rerr != nil {
		return result.Status, nil, fmt.Errorf("batch: read response: %w", rerr)
	}
	return result.Status, out, nil
}
