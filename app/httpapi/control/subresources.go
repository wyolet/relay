// subresources.go binds the catalogview read projections to chi/huma as
// resource-navigation endpoints — "API UX":
//
//	GET /models/{ref}/hosts     hosts serving this model (+ binding + pricing)
//	GET /models/{ref}/pricing   pricing per host for this model
//	GET /models/{ref}/policies  policies granting this model (+ per-model limits)
//	GET /hosts/{ref}/models      models this host serves (+ binding + pricing)
//	GET /hosts/{ref}/keys        upstream credentials this host owns (secret-free)
//	GET /hosts/{ref}/policies    host-tier serving policies this host owns
//	GET /policies/{ref}/models       models this policy grants (+ per-model limits)
//	GET /policies/{ref}/hosts        hosts this policy reaches (+ host-keys)
//	GET /policies/{ref}/rate-limits  rate-limit rule sets this policy references
//	GET /pools/{ref}/keys            the pool's keys in pool order (secret-free)
//	GET /accounts/{ref}/keys         host keys belonging to this account
//
// All composition lives in app/catalogview (PG-backed, full state incl.
// disabled rows). These handlers are thin: build the Service from the stores,
// call the projection, return its rows. {ref} is a slug or UUID id.

package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalogview"
	"github.com/wyolet/relay/app/meta"
)

// viewService builds the read-projection service for one request. When the
// configured Authorizer scopes reads, the service drops rows the caller may
// not see (foreign policies, foreign host-keys) and 404s policy-rooted
// projections on invisible policies.
func viewService(ctx context.Context, d Deps) *catalogview.Service {
	svc := &catalogview.Service{
		Models:     d.Stores.Model,
		Hosts:      d.Stores.Host,
		Bindings:   d.Stores.Binding,
		Pricings:   d.Stores.Pricing,
		Policies:   d.Stores.Policy,
		RateLimits: d.Stores.RateLimit,
		Providers:  d.Stores.Provider,
		HostKeys:   d.Stores.HostKey,
		Pools:      d.Stores.Pool,
		Accounts:   d.Stores.Account,
	}
	if s, ok := d.Authz.(authz.Scoper); ok {
		svc.Visible = func(kind, id string, owner meta.Owner) bool {
			return s.Visible(ctx, kind, id, owner)
		}
	}
	return svc
}

func notFound(err error, msg string) error {
	if errors.Is(err, catalogview.ErrNotFound) {
		return huma.Error404NotFound(msg)
	}
	return huma.Error500InternalServerError(err.Error())
}

type modelHostsOut struct {
	Body struct {
		Model catalogview.ModelRef       `json:"model" doc:"The model."`
		Hosts []catalogview.ModelHostRow `json:"hosts" doc:"Hosts bound to the model, disabled ones included."`
	}
}
type modelPricingOut struct {
	Body struct {
		Model   catalogview.ModelRef        `json:"model"   doc:"The model."`
		Pricing []catalogview.ModelPriceRow `json:"pricing" doc:"One pricing sheet per host that prices the model."`
	}
}
type modelPoliciesOut struct {
	Body struct {
		Model    catalogview.ModelRef         `json:"model"    doc:"The model."`
		Policies []catalogview.ModelPolicyRow `json:"policies" doc:"Policies the caller may see that allow the model."`
	}
}
type hostModelsOut struct {
	Body struct {
		Host   catalogview.HostRef        `json:"host"   doc:"The host."`
		Models []catalogview.HostModelRow `json:"models" doc:"Models bound to the host."`
	}
}
type hostKeysOut struct {
	Body struct {
		Host catalogview.HostRef       `json:"host" doc:"The host."`
		Keys []catalogview.HostKeyView `json:"keys" doc:"Host keys for the host the caller may see, secret-free."`
	}
}
type hostPoliciesOut struct {
	Body struct {
		Host     catalogview.HostRef         `json:"host"     doc:"The host."`
		Policies []catalogview.HostPolicyRow `json:"policies" doc:"Policies the host owns: its tier presets."`
	}
}
type policyModelsInput struct {
	Ref   string `path:"ref" doc:"Policy slug or UUIDv7 id."`
	Debug bool   `query:"debug" doc:"Also return the models this policy does NOT grant, each with the reason."`
}
type policyModelsOut struct {
	Body struct {
		Policy   catalogview.PolicyRef              `json:"policy"             doc:"The policy."`
		Models   []catalogview.PolicyBindingRow     `json:"models"             doc:"One row per (model, host) binding the policy allows, with the limits it applies there."`
		Excluded []catalogview.PolicyModelExclusion `json:"excluded,omitempty" doc:"With debug=true: the models it does not allow, each with the reason."`
	}
}
type policyHostsOut struct {
	Body struct {
		Policy catalogview.PolicyRef       `json:"policy" doc:"The policy."`
		Hosts  []catalogview.PolicyHostRow `json:"hosts"  doc:"Hosts serving a model the policy allows, with the host keys that reach each."`
	}
}
type policyRateLimitsOut struct {
	Body struct {
		Policy      catalogview.PolicyRef            `json:"policy"      doc:"The policy."`
		RateLimits  []catalogview.PolicyRateLimitRow `json:"rateLimits"  doc:"The rate limits the policy links."`
		Unthrottled []catalogview.UnthrottledModel   `json:"unthrottled" doc:"Allowed models no linked rate limit covers."`
	}
}

func registerSubresources(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "model_hosts", Method: "GET", Path: "/models/{ref}/hosts",
		Summary:     "List the hosts that serve this model, with binding + pricing",
		Description: "Every host bound to the model, disabled ones included, with the binding and its pricing. {ref} is a slug or id.",
		Tags:        []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelHostsOut, error) {
		m, rows, err := viewService(ctx, d).ModelHosts(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelHostsOut{}
		out.Body.Model, out.Body.Hosts = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_pricing", Method: "GET", Path: "/models/{ref}/pricing",
		Summary:     "List this model's pricing per host",
		Description: "The pricing sheet each host applies to the model. {ref} is a slug or id.",
		Tags:        []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelPricingOut, error) {
		m, rows, err := viewService(ctx, d).ModelPricing(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelPricingOut{}
		out.Body.Model, out.Body.Pricing = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_policies", Method: "GET", Path: "/models/{ref}/policies",
		Summary:     "List the policies that grant this model, with the limits each applies to it",
		Description: "Policies the caller may see whose model list allows the model, host tier presets included, each with the rate-limit rules it applies to the model. {ref} is a slug or id.",
		Tags:        []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelPoliciesOut, error) {
		m, rows, err := viewService(ctx, d).ModelPolicies(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelPoliciesOut{}
		out.Body.Model, out.Body.Policies = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_models", Method: "GET", Path: "/hosts/{ref}/models",
		Summary:     "List the models this host serves, with binding + pricing",
		Description: "Every model bound to the host, with the binding and its pricing. {ref} is a slug or id.",
		Tags:        []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostModelsOut, error) {
		h, rows, err := viewService(ctx, d).HostModels(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostModelsOut{}
		out.Body.Host, out.Body.Models = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_keys", Method: "GET", Path: "/hosts/{ref}/keys",
		Summary:     "List the upstream credentials this host owns (secret-free)",
		Description: "Host keys that authenticate to the host, without secrets. Keys the caller may not see are left out. {ref} is a slug or id.",
		Tags:        []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostKeysOut, error) {
		h, rows, err := viewService(ctx, d).HostKeyList(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostKeysOut{}
		out.Body.Host, out.Body.Keys = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_policies", Method: "GET", Path: "/hosts/{ref}/policies",
		Summary:     "List the host-tier serving policies this host owns, with limits",
		Description: "The host's tier presets: policies the host owns, ready to bind to an account, host key or the host itself. Each lists the rate limits it links. {ref} is a slug or id.",
		Tags:        []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostPoliciesOut, error) {
		h, rows, err := viewService(ctx, d).HostPolicies(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostPoliciesOut{}
		out.Body.Host, out.Body.Policies = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_models", Method: "GET", Path: "/policies/{ref}/models",
		Summary:     "List the models this policy grants, with the limits it applies to each",
		Description: "One row per (model, host) binding the policy allows and reaches through its pools and keys (a tier preset reaches its own host), with the matching model entries and the rate-limit rules. debug=true adds the models it does not allow, with why. A policy the caller may not see answers 404. {ref} is a slug or id.",
		Tags:        []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *policyModelsInput) (*policyModelsOut, error) {
		svc := viewService(ctx, d)
		p, rows, err := svc.PolicyModels(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "policy not found")
		}
		out := &policyModelsOut{}
		out.Body.Policy, out.Body.Models = p, rows
		if in.Debug {
			if _, excl, err := svc.PolicyModelExclusions(ctx, in.Ref); err == nil {
				out.Body.Excluded = excl
			}
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_hosts", Method: "GET", Path: "/policies/{ref}/hosts",
		Summary:     "List the hosts this policy can reach, with the host-keys that reach each",
		Description: "Hosts the policy's pools and host keys reach, each with those keys and whether it alone serves some allowed model. A policy the caller may not see answers 404. {ref} is a slug or id.",
		Tags:        []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*policyHostsOut, error) {
		p, rows, err := viewService(ctx, d).PolicyHosts(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "policy not found")
		}
		out := &policyHostsOut{}
		out.Body.Policy, out.Body.Hosts = p, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_rate_limits", Method: "GET", Path: "/policies/{ref}/rate-limits",
		Summary:     "List the rate-limit rule sets this policy references",
		Description: "The rate limits the policy links, each with its model scope and rules, and the allowed models no linked limit covers. A policy the caller may not see answers 404. {ref} is a slug or id.",
		Tags:        []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*policyRateLimitsOut, error) {
		p, view, err := viewService(ctx, d).PolicyRateLimits(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "policy not found")
		}
		out := &policyRateLimitsOut{}
		out.Body.Policy = p
		out.Body.RateLimits, out.Body.Unthrottled = view.RateLimits, view.Unthrottled
		return out, nil
	})
	registerUpstreamSubresources(api, d, protect)
}
