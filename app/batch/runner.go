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
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// Runner executes one batch item through the realtime inference pipeline,
// buffering the response. It deliberately reuses app/pipeline.Pipeline.Run so a
// batch item inherits key selection, circuit breakers, limits, failover, and usage
// emission for free — the only difference from a live request is that the
// lifecycle source is "batch" and the response is buffered rather than streamed.
type Runner struct {
	// Auth re-resolves the submitter against the current plans when the item runs.
	Auth     *inference.Auth
	Pipeline *pipeline.Pipeline
	Specs    *adapter.Registry
}

// ErrCrossShape is returned when a batch item's inbound shape differs from the
// resolved upstream adapter. v1 batch supports same-shape / byte-pass / native
// only; cross-shape translation is a follow-up. Surfaced explicitly rather than
// silently mis-dispatched (canonical rule 11).
var ErrCrossShape = errors.New("batch: cross-shape dispatch not yet supported")

// ErrCredentialInvalid is returned when the credential that submitted an item
// has since been revoked, expired, disabled, or rotated out, or its caller
// ladder now refuses it.
var ErrCredentialInvalid = errors.New("batch: credential no longer valid")

// Run executes one item. requestID ties the usage event to the item (the jobq
// job id); tok carries the submitting token's claims (zero for a key) and attr
// the submission's attribution as recorded at submit. The submitter is
// resolved again against the current plans, so the item runs under the
// caller ladder of its execution time, not of its submission. It returns the
// upstream status and the buffered response body. Usage emits automatically
// with source="batch" when the pipeline body closes.
func (rn *Runner) Run(ctx context.Context, requestID, relayKeyHash string, tok TokenClaims, attr Attribution, inbound adapters.Name, body []byte) (int, []byte, error) {
	userID := ""
	if attr.PrincipalKind == string(key.PrincipalUser) {
		userID = attr.PrincipalID
	}
	p, err := rn.Auth.Recheck(relayKeyHash, userID, tok.JTI, tok.Version, tok.Expires, time.Now())
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrCredentialInvalid, err)
	}
	if r := p.Caller.Refused; r != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrCredentialInvalid, r)
	}

	req, err := inference.ParseRequest(body)
	if err != nil {
		return 0, nil, fmt.Errorf("batch: parse model: %w", err)
	}
	modelName := req.Model
	route := p.Caller.Routes.Get(modelName)
	if route.Refused != nil {
		return 0, nil, fmt.Errorf("batch: route %q: %w", modelName, route.Refused)
	}

	first := &route.Hops[0]
	shape := first.Binding.Spec.Adapter
	inboundSpec := rn.Specs.Spec(inbound)
	upstreamSpec := rn.Specs.Spec(shape)
	upstreamAdapter := rn.Specs.PipelineAdapter(shape)
	if inboundSpec == nil || upstreamSpec == nil || upstreamAdapter == nil {
		return 0, nil, fmt.Errorf("batch: adapter not registered for %s/%s", inbound, shape)
	}
	native := inboundSpec.IsNativePath
	sameShape := inbound == shape || inboundSpec.BytePass || (native != nil && native(first.Binding, first.Host))
	if !sameShape {
		return 0, nil, fmt.Errorf("%w (%s→%s)", ErrCrossShape, inbound, shape)
	}
	// One body serves every hop, so failover keeps to the hops that speak the first hop's shape.
	hops := make([]plan.RouteHop, 0, len(route.Hops))
	for _, h := range route.Hops {
		if h.Binding.Spec.Adapter == shape {
			hops = append(hops, h)
		}
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
	// The ids were fixed at submit; the names describe them as of execution, from the plan the item runs under.
	cp := p.Caller
	if cp.Project().ID() == attr.ProjectID {
		lc.ProjectName = cp.Project().Name
	}
	if cp.Team().ID() == attr.TeamID {
		lc.TeamName = cp.Team().Name
	}
	if attr.PrincipalKind == string(key.PrincipalServiceAccount) {
		lc.PrincipalName = cp.PrincipalRung().Name
	}
	if p.Key != nil {
		lc.RelayKeyName = p.Key.Meta.Name
	}
	if pol := p.Policy(); pol != nil {
		lc.PolicyID, lc.PolicyName = pol.Meta.ID, pol.Meta.Name
	}
	lc.ModelID, lc.ModelName, lc.ProviderName = route.Model.Meta.ID, route.Model.Meta.Name, route.Provider
	if route.ResolvedVia != "" {
		lc.Metadata["resolved_via"] = route.ResolvedVia
	}
	lc.Translator = upstreamSpec.Translator

	// Same-shape only (guarded above), so the body stays inbound-shaped and
	// the inbound spec's ParamPaths address it — mirrors runBytePass.
	wireBody, _ := inference.UpstreamBody(req, route.UpstreamModel, route.Model, inboundSpec.ParamPaths)

	result, err := rn.Pipeline.Run(ctx, &pipeline.Request{
		Body:          wireBody,
		Headers:       http.Header{},
		Adapter:       upstreamAdapter,
		Model:         route.Model,
		Provider:      route.Provider,
		Hops:          hops,
		ModelName:     route.Model.Meta.Name,
		UpstreamModel: route.UpstreamModel,
		Lifecycle:     lc,
	})
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
