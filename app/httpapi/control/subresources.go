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
//
// All composition lives in app/catalogview (PG-backed, full state incl.
// disabled rows). These handlers are thin: build the Service from the stores,
// call the projection, return its rows. {ref} is a slug or UUID id.
package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/catalogview"
)

func viewService(d Deps) *catalogview.Service {
	return &catalogview.Service{
		Models:     d.Stores.Model,
		Hosts:      d.Stores.Host,
		Bindings:   d.Stores.Binding,
		Pricings:   d.Stores.Pricing,
		Policies:   d.Stores.Policy,
		RateLimits: d.Stores.RateLimit,
		Providers:  d.Stores.Provider,
		HostKeys:   d.Stores.HostKey,
	}
}

func notFound(err error, msg string) error {
	if errors.Is(err, catalogview.ErrNotFound) {
		return huma.Error404NotFound(msg)
	}
	return huma.Error500InternalServerError(err.Error())
}

type modelHostsOut struct {
	Body struct {
		Model catalogview.ModelRef       `json:"model"`
		Hosts []catalogview.ModelHostRow `json:"hosts"`
	}
}
type modelPricingOut struct {
	Body struct {
		Model   catalogview.ModelRef        `json:"model"`
		Pricing []catalogview.ModelPriceRow `json:"pricing"`
	}
}
type modelPoliciesOut struct {
	Body struct {
		Model    catalogview.ModelRef         `json:"model"`
		Policies []catalogview.ModelPolicyRow `json:"policies"`
	}
}
type hostModelsOut struct {
	Body struct {
		Host   catalogview.HostRef        `json:"host"`
		Models []catalogview.HostModelRow `json:"models"`
	}
}
type hostKeysOut struct {
	Body struct {
		Host catalogview.HostRef       `json:"host"`
		Keys []catalogview.HostKeyView `json:"keys"`
	}
}
type hostPoliciesOut struct {
	Body struct {
		Host     catalogview.HostRef         `json:"host"`
		Policies []catalogview.HostPolicyRow `json:"policies"`
	}
}
type policyModelsInput struct {
	Ref   string `path:"ref" doc:"Policy slug or UUIDv7 id."`
	Debug bool   `query:"debug" doc:"Also return the models this policy does NOT grant, each with the reason."`
}
type policyModelsOut struct {
	Body struct {
		Policy   catalogview.PolicyRef              `json:"policy"`
		Models   []catalogview.PolicyBindingRow     `json:"models"`
		Excluded []catalogview.PolicyModelExclusion `json:"excluded,omitempty"`
	}
}
type policyHostsOut struct {
	Body struct {
		Policy catalogview.PolicyRef       `json:"policy"`
		Hosts  []catalogview.PolicyHostRow `json:"hosts"`
	}
}
type policyRateLimitsOut struct {
	Body struct {
		Policy     catalogview.PolicyRef            `json:"policy"`
		RateLimits []catalogview.PolicyRateLimitRow `json:"rateLimits"`
	}
}

func registerSubresources(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "model_hosts", Method: "GET", Path: "/models/{ref}/hosts",
		Summary: "List the hosts that serve this model, with binding + pricing",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelHostsOut, error) {
		m, rows, err := viewService(d).ModelHosts(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelHostsOut{}
		out.Body.Model, out.Body.Hosts = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_pricing", Method: "GET", Path: "/models/{ref}/pricing",
		Summary: "List this model's pricing per host",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelPricingOut, error) {
		m, rows, err := viewService(d).ModelPricing(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelPricingOut{}
		out.Body.Model, out.Body.Pricing = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_policies", Method: "GET", Path: "/models/{ref}/policies",
		Summary: "List the policies that grant this model, with the limits each applies to it",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelPoliciesOut, error) {
		m, rows, err := viewService(d).ModelPolicies(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "model not found")
		}
		out := &modelPoliciesOut{}
		out.Body.Model, out.Body.Policies = m, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_models", Method: "GET", Path: "/hosts/{ref}/models",
		Summary: "List the models this host serves, with binding + pricing",
		Tags:    []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostModelsOut, error) {
		h, rows, err := viewService(d).HostModels(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostModelsOut{}
		out.Body.Host, out.Body.Models = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_keys", Method: "GET", Path: "/hosts/{ref}/keys",
		Summary: "List the upstream credentials this host owns (secret-free)",
		Tags:    []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostKeysOut, error) {
		h, rows, err := viewService(d).HostKeyList(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostKeysOut{}
		out.Body.Host, out.Body.Keys = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_policies", Method: "GET", Path: "/hosts/{ref}/policies",
		Summary: "List the host-tier serving policies this host owns, with limits",
		Tags:    []string{"hosts"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostPoliciesOut, error) {
		h, rows, err := viewService(d).HostPolicies(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "host not found")
		}
		out := &hostPoliciesOut{}
		out.Body.Host, out.Body.Policies = h, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_models", Method: "GET", Path: "/policies/{ref}/models",
		Summary: "List the models this policy grants, with the limits it applies to each",
		Tags:    []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *policyModelsInput) (*policyModelsOut, error) {
		svc := viewService(d)
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
		Summary: "List the hosts this policy can reach, with the host-keys that reach each",
		Tags:    []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*policyHostsOut, error) {
		p, rows, err := viewService(d).PolicyHosts(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "policy not found")
		}
		out := &policyHostsOut{}
		out.Body.Policy, out.Body.Hosts = p, rows
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "policy_rate_limits", Method: "GET", Path: "/policies/{ref}/rate-limits",
		Summary: "List the rate-limit rule sets this policy references",
		Tags:    []string{"policies"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*policyRateLimitsOut, error) {
		p, rows, err := viewService(d).PolicyRateLimits(ctx, in.Ref)
		if err != nil {
			return nil, notFound(err, "policy not found")
		}
		out := &policyRateLimitsOut{}
		out.Body.Policy, out.Body.RateLimits = p, rows
		return out, nil
	})
}
