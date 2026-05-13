// Package routing implements model and route resolution for the request hot path.
// It translates an incoming (RouteHeader, ModelName) pair into a fully-populated
// RequestPlan that the pipeline can execute.
//
// Judgment call: RequestPlan is defined here (in internal/routing) rather than in
// internal/api/openai. This avoids the import cycle that would arise if routing
// imported internal/api/openai (which now imports routing). internal/api/openai
// re-exports RequestPlan as a type alias so existing call sites don't break.
package routing

import (
	"errors"

	"github.com/wyolet/relay/internal/catalog"
)

// RequestPlan holds the resolved model, provider, policy, secrets, and rate-limit
// rules for a single request. Rules are pre-resolved for Policy+Model scope at plan
// time; secret-level rules are M4+ work.
type RequestPlan struct {
	Model    *catalog.Model
	Provider *catalog.Provider
	Policy     *catalog.Policy
	Secrets  []*catalog.Secret
	Rules    []catalog.ResolvedRule
	// Passthrough is true when the request should forward the inbound
	// Authorization header to upstream instead of using a relay-managed
	// secret. Populated by the HTTP handler from the auth.Subject; routing
	// no longer reads policy-level passthrough (that field was removed in #80).
	Passthrough bool
	// PassthroughAuth is the inbound Authorization header value to forward
	// (includes the "Bearer " prefix). Set by the HTTP handler from
	// auth.Subject.PassthroughAuth.
	PassthroughAuth string
	// PassthroughHeaders are additional inbound headers to forward for passthrough policies.
	// Set by the HTTP handler from the subset of inbound headers in OutboundPassthroughExtra.
	PassthroughHeaders map[string]string
	// RawQuery is the inbound request query string, forwarded to upstream.
	RawQuery string
}

// Catalog is the narrow read-only view that the resolver needs from the catalog.
// It is defined on the consumer side so that *catalog.MemStore / *catalog.PGStore
// satisfy it implicitly via Go duck typing — no changes to catalog required.
type Catalog interface {
	DefaultRoute() *catalog.Route
	RouteByName(name string) (*catalog.Route, bool)
	ModelByName(name string) (*catalog.Model, bool)
	ModelByID(id string) (*catalog.Model, bool)
	ProviderForModel(modelName string) (*catalog.Provider, bool)
	PolicyByName(name string) (*catalog.Policy, bool)
	PolicyByID(id string) (*catalog.Policy, bool)
	SecretsForPolicy(policy *catalog.Policy) []*catalog.Secret
	RateLimitsForRequest(provider *catalog.Provider, policy *catalog.Policy, model *catalog.Model, secret *catalog.Secret) []catalog.ResolvedRule
	Passthrough() *catalog.Passthrough
}

// Sentinel errors returned by Resolve.
var (
	ErrUnknownRoute     = errors.New("routing: unknown route")
	ErrModelNotInRoute  = errors.New("routing: model not in route")
	ErrUnknownModel     = errors.New("routing: unknown model")
	ErrNoModelSpecified = errors.New("routing: no model specified")
	ErrUnknownProvider  = errors.New("routing: unknown provider")
	ErrUnknownPolicy      = errors.New("routing: unknown policy")
	ErrModelNotAllowed    = errors.New("routing: model not allowed by policy")
)

// Request is what the HTTP layer hands to the resolver.
type Request struct {
	RouteHeader string // value of X-Relay-Route; "" if header absent
	ModelName   string // from parsed request body; "" if missing
	// PolicyOverride, when non-empty, forces buildPlan to use the named Policy
	// in place of the provider's defaultPolicy. Sourced from the authenticated
	// RelayKey's PolicyRef.
	PolicyOverride string
}

// Resolver resolves a routing.Request into a RequestPlan.
type Resolver struct {
	catalog Catalog
}

// New constructs a Resolver backed by the supplied Catalog.
func New(c Catalog) *Resolver {
	return &Resolver{catalog: c}
}

// Passthrough returns the current passthrough config (singleton). Handlers
// use this to enforce the models allowlist on BYO-credential requests.
func (res *Resolver) Passthrough() *catalog.Passthrough {
	return res.catalog.Passthrough()
}

// Resolve applies the routing precedence rules and returns a fully-populated
// RequestPlan, or one of the sentinel errors defined in this package.
//
// Precedence:
//  1. X-Relay-Route header present → named route lookup; model must be in route (or empty → first).
//  2. Model name in body only → direct model lookup (legacy behavior).
//  3. Neither → use the default route's first model, or ErrNoModelSpecified.
func (res *Resolver) Resolve(req Request) (*RequestPlan, error) {
	modelName, err := res.pickModel(req)
	if err != nil {
		return nil, err
	}
	return res.buildPlan(modelName, req.PolicyOverride)
}

// pickModel applies the three-level precedence and returns the resolved model name.
func (res *Resolver) pickModel(req Request) (string, error) {
	if req.RouteHeader != "" {
		route, ok := res.catalog.RouteByName(req.RouteHeader)
		if !ok || !catalog.IsEnabled(route.Spec.Enabled) {
			return "", ErrUnknownRoute
		}
		if req.ModelName != "" {
			if !res.modelInRoute(req.ModelName, route) {
				return "", ErrModelNotInRoute
			}
			return req.ModelName, nil
		}
		if len(route.Spec.Models) == 0 {
			return "", ErrNoModelSpecified
		}
		return res.modelNameByID(route.Spec.Models[0])
	}

	if req.ModelName != "" {
		return req.ModelName, nil
	}

	// Neither header nor body model: use default route.
	dr := res.catalog.DefaultRoute()
	if dr == nil || len(dr.Spec.Models) == 0 {
		return "", ErrNoModelSpecified
	}
	return res.modelNameByID(dr.Spec.Models[0])
}

// modelNameByID renders a Model id to its canonical slug. Used when picking
// the first model from a Route — RouteSpec.Models[] is stored as ids, but the
// rest of the pipeline still keys lookups by name (and includes aliases).
func (res *Resolver) modelNameByID(id string) (string, error) {
	m, ok := res.catalog.ModelByID(id)
	if !ok {
		return "", ErrUnknownModel
	}
	return m.Metadata.Name, nil
}

// buildPlan resolves provider, policy, secrets, and rate limits for a model name.
// policyOverride, when non-empty, replaces the provider's defaultPolicy.
func (res *Resolver) buildPlan(modelName, policyOverride string) (*RequestPlan, error) {
	m, ok := res.catalog.ModelByName(modelName)
	if !ok || !catalog.IsEnabled(m.Spec.Enabled) {
		return nil, ErrUnknownModel
	}
	p, ok := res.catalog.ProviderForModel(modelName)
	if !ok || !catalog.IsEnabled(p.Spec.Enabled) {
		return nil, ErrUnknownProvider
	}
	plan := &RequestPlan{Model: m, Provider: p}
	// Both DefaultPolicy and the RelayKey-sourced PolicyOverride carry the
	// canonical Policy id after the snapshot resolver runs.
	policyID := p.Spec.DefaultPolicy
	if policyOverride != "" {
		policyID = policyOverride
	}
	if policyID != "" {
		policy, ok := res.catalog.PolicyByID(policyID)
		if !ok || !catalog.IsEnabled(policy.Spec.Enabled) {
			return nil, ErrUnknownPolicy
		}
		if len(policy.Spec.Models) > 0 && !res.modelAllowed(modelName, policy.Spec.Models) {
			return nil, ErrModelNotAllowed
		}
		plan.Policy = policy
		plan.Secrets = res.catalog.SecretsForPolicy(policy)
		plan.Rules = res.catalog.RateLimitsForRequest(p, policy, m, nil)
	}
	return plan, nil
}

// modelInRoute reports whether the model resolved from modelName (slug or
// alias) appears in route.Spec.Models. RouteSpec.Models[] carries Model ids,
// so we resolve the incoming name to its model and compare by id.
func (res *Resolver) modelInRoute(modelName string, route *catalog.Route) bool {
	m, ok := res.catalog.ModelByName(modelName)
	if !ok {
		return false
	}
	for _, id := range route.Spec.Models {
		if id == m.Metadata.ID {
			return true
		}
	}
	return false
}

// modelAllowed is the Policy.Spec.Models analogue of modelInRoute.
func (res *Resolver) modelAllowed(modelName string, allowed []string) bool {
	m, ok := res.catalog.ModelByName(modelName)
	if !ok {
		return false
	}
	for _, id := range allowed {
		if id == m.Metadata.ID {
			return true
		}
	}
	return false
}
