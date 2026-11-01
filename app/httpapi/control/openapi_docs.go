package control

import (
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
)

// The 409 answers of the generic CRUD operations, by operation.
const (
	createConflicts = "A conflict with stored rows: duplicate_binding (a policy-binding subject another binding already holds), managed_by_source (a provisioning source owns the row), or a second default account for one owner and host."
	updateConflicts = "stale_resource_version (metadata.resourceVersion no longer matches; reload and reapply), resource_in_use (an owner move would strand referencing rows; blockers lists them), managed_by_source, or duplicate_binding."
	deleteConflicts = "resource_in_use: other rows still reference the target; blockers lists them, and groups marked detachable can be released with POST …/detach. Also managed_by_source for a row a provisioning source owns."
)

// controlTags describes each tag the control operations use, one per kind or area.
var controlTags = []*huma.Tag{
	{Name: "accounts", Description: "Accounts: one provider organization at one host. Host keys of an account share its upstream limits; bind a tier policy to the account to cap them. Every key joins its owner's default account at its host unless it names one."},
	{Name: "audit", Description: "The admin audit log."},
	{Name: "auth", Description: "Sign-in, sessions, inference tokens and access review."},
	{Name: "budgets", Description: "Budgets: recurring USD spend caps over a calendar period. A policy links budgets; every request under it must fit each one."},
	{Name: "catalog", Description: "Read views over the model catalog for pickers and model-reference resolution."},
	{Name: "debug", Description: "Operator diagnostics."},
	{Name: "groups", Description: "Groups of users, local or provisioned by SCIM. Role bindings can name a group; policy bindings cannot."},
	{Name: "host-bindings", Description: "Host bindings: which host serves which model, over which wire shape, at what price. One per (model, host) pair."},
	{Name: "host-keys", Description: "Host keys: credentials relay uses to call a host. Each belongs to an account at its host; pools and policies link keys by id."},
	{Name: "hosts", Description: "Hosts: serving endpoints (a provider's API, a cloud, a self-hosted server). Host-owned policies are the host's tier presets."},
	{Name: "keys", Description: "Relay keys: bearer credentials callers send to the inference API. A key acts as its principal (a service account or a user); the policies bound to that principal and its project and team govern it."},
	{Name: "logs", Description: "Per-request log records with captured bodies."},
	{Name: "models", Description: "Models in the catalog, with their snapshots, capabilities and aliases."},
	{Name: "policies", Description: "Policies: the gate between a subject and what it may use. A policy links models, pools, host keys, rate limits and budgets; a policy binding attaches it to subjects. Every rung a request passes (host, account, host key, team, project, principal) may have one policy, and each only narrows."},
	{Name: "policy-bindings", Description: "Policy bindings: attach one policy to subjects inside the binding owner's scope (system, team or project). A subject is held by at most one binding; a user once per scope. Only system-owned bindings may name a host."},
	{Name: "pools", Description: "Pools: ordered sets of host keys at one host, with a selection algorithm. Policies draw keys from pools, tried in order; nothing in a pool is reserved for one policy."},
	{Name: "pricings", Description: "Pricing sheets: a host's rates for a set of models."},
	{Name: "projects", Description: "Projects inside a team; service accounts and project keys live here."},
	{Name: "providers", Description: "Providers: the vendors whose models the catalog lists."},
	{Name: "rate-limits", Description: "Rate limits: rule sets a policy links. Each rule counts one meter over a window."},
	{Name: "role-bindings", Description: "Role bindings: grant a role to users, groups or service accounts at a scope."},
	{Name: "roles", Description: "Roles: named sets of rules over control-API kinds and verbs."},
	{Name: "scim", Description: "SCIM provisioning clients."},
	{Name: "secret-keys", Description: "The data keys stored secrets are sealed under."},
	{Name: "service-accounts", Description: "Service accounts: non-human principals inside a project, holding relay keys."},
	{Name: "settings", Description: "Typed, hot-reloaded settings sections."},
	{Name: "system", Description: "Health, version, license, manifests and operator actions."},
	{Name: "teams", Description: "Teams: the top tenancy scope, holding projects."},
	{Name: "usage", Description: "Usage events and aggregates."},
	{Name: "users", Description: "User accounts."},
}

// schemaOf registers v's type and returns a reference to its schema.
func schemaOf(api huma.API, v any) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(v), true, "")
}

// jsonResponse documents a JSON response whose body is v's type.
func jsonResponse(api huma.API, desc string, v any) *huma.Response {
	return &huma.Response{Description: desc, Content: map[string]*huma.MediaType{"application/json": {Schema: schemaOf(api, v)}}}
}

// conflictResponse documents a 409, whose body may carry blockers.
func conflictResponse(api huma.API, desc string) *huma.Response {
	return jsonResponse(api, desc, conflictBody{})
}

// errorDescriptions replaces huma's bare status texts on error responses.
var errorDescriptions = map[int]string{
	http.StatusBadRequest:          "The request breaks a rule the schema cannot express, such as a cross-row reference or an unknown filter parameter; error.message says which.",
	http.StatusUnauthorized:        "No valid session cookie or admin bearer token.",
	http.StatusForbidden:           "Authenticated, but a role, a governance setting or a scope rule does not allow it.",
	http.StatusNotFound:            "No such row, or one the caller may not see.",
	http.StatusConflict:            "The request conflicts with the stored state; error.code says how.",
	http.StatusUnprocessableEntity: "The request does not match the schema; error.details lists each failing field.",
	http.StatusTooManyRequests:     "Too many requests; retry after the Retry-After header.",
	http.StatusInternalServerError: "The server failed; error.message carries the cause.",
	http.StatusNotImplemented:      "This deployment has not wired the backend the operation needs.",
	http.StatusServiceUnavailable:  "A backend the operation needs is not configured or not reachable.",
}

// describeErrors gives every error response that still carries a bare status text a description of what it means on this API.
func describeErrors(api huma.API) {
	for _, item := range api.OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil {
				continue
			}
			for code, resp := range op.Responses {
				status, err := strconv.Atoi(code)
				if err != nil || resp.Description != http.StatusText(status) {
					continue
				}
				if d, ok := errorDescriptions[status]; ok {
					resp.Description = d
				}
			}
		}
	}
}

// entityEnvelope documents the metadata and spec every kind carries, on every schema that holds both.
func entityEnvelope(api huma.API) {
	for _, s := range api.OpenAPI().Components.Schemas.Map() {
		md, spec := s.Properties["metadata"], s.Properties["spec"]
		if md == nil || spec == nil || md.Ref != "#/components/schemas/Metadata" {
			continue
		}
		if md.Description == "" {
			md.Description = "Identity and provenance: id, name, owner, labels."
		}
		if spec.Description == "" {
			spec.Description = "The row's configuration."
		}
	}
}

// policyBindingSubject gives policy-binding subjects their own schema: the Go type is shared with role bindings, whose kinds differ.
func policyBindingSubject(api huma.API) {
	reg := api.OpenAPI().Components.Schemas
	spec := reg.SchemaFromRef("#/components/schemas/PolicyBindingSpec")
	if spec == nil || spec.Properties["subjects"] == nil {
		return
	}
	kinds := make([]any, len(policybinding.SubjectKinds))
	for i, k := range policybinding.SubjectKinds {
		kinds[i] = string(k)
	}
	kind := &huma.Schema{
		Type:        huma.TypeString,
		Enum:        kinds,
		Description: "What the subject is: a principal (user, serviceaccount), a tenancy scope (team, project), or an upstream rung (host, account, credential, meaning a host key). Groups are not policy subjects.",
	}
	id := &huma.Schema{Type: huma.TypeString, Description: "Id of the subject row."}
	subject := &huma.Schema{
		Type:                 huma.TypeObject,
		Description:          "One rung a policy binding governs.",
		Properties:           map[string]*huma.Schema{"kind": kind, "id": id},
		Required:             []string{"kind", "id"},
		AdditionalProperties: false,
	}
	for _, s := range []*huma.Schema{kind, id, subject} {
		s.PrecomputeMessages()
	}
	reg.Map()["PolicyBindingSubject"] = subject
	spec.Properties["subjects"].Items = &huma.Schema{Ref: "#/components/schemas/PolicyBindingSubject"}
}

// stringEnum projects a closed set onto huma's enum form.
func stringEnum[S ~string](set []S) []any {
	out := make([]any, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}

// documentDomainSchemas adds what the domain types carry no tags for: the rate-limit shape, closed sets kept as data, and types owned by the sdk and secret modules.
func documentDomainSchemas(api huma.API) {
	describe := func(schema, prop, desc string) {
		patchProp(api, schema, prop, func(s *huma.Schema) { s.Description = desc })
	}
	itemsEnum := func(schema, prop string, enum []any) {
		patchProp(api, schema, prop, func(s *huma.Schema) {
			if s.Items != nil {
				s.Items.Enum = enum
				s.Items.PrecomputeMessages()
			}
		})
	}

	describe("RateLimitSpec", "rules", "Rules a request must fit, each counted separately. An enabled rate limit needs at least one; no two may share meter, strategy and window.")
	describe("RateLimitSpec", "enabled", "False stops enforcing the rules. Omitted means true.")
	describe("RateLimitRule", "meter", "What the rule counts: requests, concurrent requests, all tokens, or one token kind.")
	describe("RateLimitRule", "amount", "How much of the meter the window allows.")
	describe("RateLimitRule", "strategy", "Counting algorithm. session-window starts its window at the first request and idles after it ends, as subscription quotas do.")
	patchProp(api, "RateLimitRule", "strategy", func(s *huma.Schema) {
		s.Enum = stringEnum([]ratelimit.Strategy{
			ratelimit.StrategyTokenBucket, ratelimit.StrategySlidingWindow, ratelimit.StrategyFixedWindow,
			ratelimit.StrategyLeakyBucket, ratelimit.StrategySessionWindow,
		})
		s.PrecomputeMessages()
	})
	itemsEnum("RoleRule", "kinds", stringEnum(role.Kinds))
	itemsEnum("RoleRule", "verbs", stringEnum(role.Verbs))

	describe("SecretRef", "kind", "Where the secret lives: env, stored, oauth, or an external backend (aws, gcp, azure, bitwarden, onepassword).")
	describe("SecretRef", "env", "Environment variable name, for kind env.")
	describe("SecretRef", "id", "Id of the stored secret, for kind stored or oauth.")
	describe("SecretRef", "path", "Backend-specific locator, for the external backends.")
	describe("SecretRef", "provider", "OAuth provider configuration that refreshes the credential, for kind oauth.")
	describe("UpstreamTiming", "start", "Microseconds from request start until the upstream call began.")
	describe("UpstreamTiming", "response_start", "Microseconds from request start until the first upstream byte.")
	describe("UpstreamTiming", "response_end", "Microseconds from request start until the upstream response ended.")
	describe("ReasoningTiming", "start", "Microseconds from request start until the first reasoning frame.")
	describe("ReasoningTiming", "end", "Microseconds from request start until the last reasoning frame.")
}
