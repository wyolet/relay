package inference

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
)

// inferenceTags describes the tags the inference operations use.
var inferenceTags = []*huma.Tag{
	{Name: "inference", Description: "Model calls in each supported wire shape, and the listings a caller's credential can see. Authenticate with a relay key or token as Authorization: Bearer."},
	{Name: "system", Description: "Health probes."},
}

// catalogDocs describes the SDK catalog document's fields; the sdk module carries no API tags.
var catalogDocs = map[string]map[string]string{
	"Catalog": {
		"version":     "Catalog release the document was built from.",
		"generatedAt": "When the document was generated.",
		"hosts":       "Hosts, each with the bindings the caller can route to.",
		"models":      "Metadata of the models the bindings name, once per model.",
		"providers":   "Metadata of the models' providers.",
	},
	"Host": {
		"name":          "Host slug.",
		"baseURL":       "Upstream root URL; left out for callers, who call through relay.",
		"path":          "Upstream path override; left out for callers.",
		"models":        "The host's bindings the caller can route to.",
		"displayName":   "Display name.",
		"description":   "Description.",
		"homepageURL":   "Host homepage.",
		"docsURL":       "Host API documentation.",
		"consoleURL":    "Where keys are managed; left out for callers.",
		"statusPageURL": "Host status page.",
		"icon":          "Icon asset path.",
	},
	"Binding": {
		"upstream":  "The model's wire name at the host, as the provider names it.",
		"model":     "The snapshot slug; send it, or upstream, as the request's model.",
		"adapter":   "Wire shape relay speaks to the host for the model.",
		"providers": "Slugs of the providers that author the model.",
		"featured":  "The catalog marks the model as featured.",
		"pricing":   "The binding's rates.",
		"aliases":   "Extra request names that resolve to this binding; one * wildcard allowed.",
	},
	"ModelInfo": {
		"metadataName":         "The snapshot slug bindings name in model.",
		"provider":             "Slug of the authoring provider.",
		"displayName":          "Display name.",
		"description":          "Description.",
		"family":               "Model family.",
		"productLine":          "The vendor's named variant inside the family.",
		"version":              "Version label.",
		"capabilities":         "What the model supports.",
		"modalities":           "Input and output modalities.",
		"contextWindowInput":   "Published input cap in tokens.",
		"contextWindowOutput":  "Published output share of the context window in tokens.",
		"contextWindowTotal":   "Context window in tokens.",
		"maxOutputTokens":      "Most tokens one response may generate.",
		"knowledgeCutoff":      "Training-data cutoff.",
		"releaseDate":          "Release date, YYYY-MM-DD.",
		"license":              "License of the weights, for open models.",
		"tags":                 "Free-form tags.",
		"documentation":        "Link to the model's documentation.",
		"providerModelPageURL": "The provider's page for the model.",
	},
	"ProviderInfo": {
		"name":          "Provider slug.",
		"displayName":   "Display name.",
		"description":   "Description.",
		"homepageURL":   "Provider homepage.",
		"docsURL":       "Provider documentation.",
		"statusPageURL": "Provider status page.",
		"icon":          "Icon asset path.",
	},
}

// describeCatalogSchemas documents the SDK catalog types: from catalogDocs, and for the types that mirror a domain type field for field, from that type's doc tags.
func describeCatalogSchemas(api huma.API) {
	reg := api.OpenAPI().Components.Schemas
	describe := func(name string, docs map[string]string) {
		s := reg.SchemaFromRef("#/components/schemas/" + name)
		if s == nil {
			return
		}
		for prop, desc := range docs {
			if p := s.Properties[prop]; p != nil && p.Description == "" {
				p.Description = desc
			}
		}
	}
	for name, docs := range catalogDocs {
		describe(name, docs)
	}
	describe("Capabilities", docTags(reflect.TypeFor[model.Capabilities]()))
	describe("Modalities", docTags(reflect.TypeFor[model.Modalities]()))
	describe("Rate", docTags(reflect.TypeFor[pricing.Rate]()))
}

// docTags maps each field's JSON name to its doc tag.
func docTags(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if doc := f.Tag.Get("doc"); name != "" && doc != "" {
			out[name] = doc
		}
	}
	return out
}

// generationDescription documents a generation route; the bodies pass through in the vendor's own shape, so the schema says little and this says how the route behaves.
func generationDescription(spec *adapter.Spec) string {
	routing := "Routes the request by its model field to a host the caller's policies allow, translating between wire shapes when the host speaks another, and answers in the shape of this path. Streams as server-sent events when the body sets stream. "
	if spec.BytePass {
		routing = "Routes the request by its model field to a host the caller's policies allow and serves it in this wire shape. "
	}
	return routing +
		"Every rung of the caller's ladder (host, account, host key, team, project, principal) may refuse: a refusal names the rung and policy in error.message. " +
		"Errors relay makes carry X-Wr-Origin: relay and the error envelope; an upstream's own error is passed through with X-Wr-Origin: upstream, X-Wr-Upstream-Status, and the upstream's body."
}

// generationErrors are the error codes each status carries on the generation routes.
var generationErrors = map[int]string{
	http.StatusBadRequest:            "The body is malformed or names no model. Codes: missing_model, parse_error, duplicate_model, invalid_model_key, duplicate_stream, invalid_stream_key, read_body, translate_request, embeddings_unsupported_host; in proxy mode also unknown_upstream_host, upstream_host_conflict, missing_upstream_host, missing_upstream_auth.",
	http.StatusUnauthorized:          "No valid relay key or token. Codes: unauthenticated (missing, invalid, expired or disabled credential), token_revoked.",
	http.StatusForbidden:             "The caller may not use this model. Codes: model_not_allowed (a policy on the ladder does not list it), model_disabled, policy_disabled, subject_disabled (the key's principal, project or team is disabled), duplicate_binding (two policy bindings govern one subject), unpriced_under_budget (a budget governs the request and the model has no pricing), passthrough_forbidden, project_unavailable, proxy_disabled.",
	http.StatusNotFound:              "Code model_not_found: no model, snapshot or alias matches the model field.",
	http.StatusRequestTimeout:        "Code request_timeout: in proxy mode, the request body stopped arriving.",
	http.StatusRequestEntityTooLarge: "Code request_too_large: the body exceeds the listener's limit.",
	http.StatusTooManyRequests:       "A limit refused the request before any upstream call. Codes: rate_limit_exceeded (a rate limit on the ladder, or every host key over its upstream limit), budget_exceeded (a budget on the ladder is spent). Retry-After says when to retry: the limit's refill, or the start of the budget's next period.",
	http.StatusInternalServerError:   "Relay is misconfigured or failed. Codes such as no_adapter, no_translator, marshal_request, serialize_response, pre_flight_aborted.",
	http.StatusBadGateway:            "The upstream failed. Codes: upstream_unreachable, upstream_unavailable, upstream_error, upstream_stalled, read_response, translate_response.",
	http.StatusServiceUnavailable:    "Relay cannot serve now. Codes: overloaded (the pod is at capacity; Retry-After set; a path whose shape defines its own overload error answers with that instead), no_keys (no host key the caller can reach serves the model), no_host_binding, not_ready, catalog_unavailable.",
}

// listingErrors are the error codes the read-only inference routes answer with.
var listingErrors = map[int]string{
	http.StatusUnauthorized:        "No valid relay key or token.",
	http.StatusForbidden:           "The credential may not use this route; error.code says why.",
	http.StatusInternalServerError: "Relay failed; error.message carries the cause.",
	http.StatusServiceUnavailable:  "The catalog is not loaded yet (not_ready, catalog_unavailable); retry shortly.",
}

func header(desc string, t string) *huma.Param {
	return &huma.Param{Description: desc, Schema: &huma.Schema{Type: t}}
}

// errorHeaders are the headers every relay-made inference error carries.
func errorHeaders() map[string]*huma.Param {
	return map[string]*huma.Param{
		HeaderOrigin:     header("relay when relay made the response, upstream when the provider's response passed through.", huma.TypeString),
		"X-Should-Retry": header("Whether retrying the identical request can succeed.", huma.TypeBoolean),
	}
}

// errorResponse documents one error status in relay's envelope.
func errorResponse(api huma.API, status int, desc string) *huma.Response {
	h := errorHeaders()
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		h["Retry-After"] = header("Seconds to wait before retrying, at least 1.", huma.TypeInteger)
	}
	return &huma.Response{
		Description: desc,
		Headers:     h,
		Content:     map[string]*huma.MediaType{"application/json": {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(httpapi.APIError{}), true, "")}},
	}
}

// generationResponses documents a generation route's answers, the shape's own overload answer included.
func generationResponses(api huma.API, spec *adapter.Spec) map[string]*huma.Response {
	ok := &huma.Response{
		Description: "The response in the path's wire shape: a JSON body, or a server-sent event stream when the request set stream.",
		Content: map[string]*huma.MediaType{
			"application/json":  {Schema: &huma.Schema{Type: huma.TypeObject, Description: "The response body in the path's wire shape."}},
			"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeString, Description: "Server-sent events in the path's wire shape."}},
		},
	}
	if spec.BytePass {
		ok.Description = "The response in the path's wire shape."
		delete(ok.Content, "text/event-stream")
	}
	out := map[string]*huma.Response{"200": ok}
	for status, desc := range generationErrors {
		out[strconv.Itoa(status)] = errorResponse(api, status, desc)
	}
	if overload := overloadedFor(spec); overload.Status != http.StatusServiceUnavailable {
		r := errorResponse(api, http.StatusServiceUnavailable, "The pod is at capacity, in this wire shape's own overload error; Retry-After set.")
		r.Content = map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: huma.TypeObject, Description: "The wire shape's overload error body."}}}
		out[strconv.Itoa(overload.Status)] = r
	}
	return out
}

// documentRequestBody adds the generation route's request body to the document only. Declared on the operation before registration, huma would read and validate the body itself, ahead of the handler's own streaming read.
func documentRequestBody(api huma.API, path string, spec *adapter.Spec) {
	if item := api.OpenAPI().Paths[path]; item != nil && item.Post != nil {
		item.Post.RequestBody = generationRequestBody(spec)
	}
}

// generationRequestBody documents the body relay reads: the model and stream fields; the rest passes on in the vendor's shape.
func generationRequestBody(spec *adapter.Spec) *huma.RequestBody {
	props := map[string]*huma.Schema{
		"model": {Type: huma.TypeString, Description: "Model to route: a model or snapshot name, provider/model, provider/model@host, or an alias."},
	}
	if !spec.BytePass {
		props["stream"] = &huma.Schema{Type: huma.TypeBoolean, Description: "Answer as a server-sent event stream."}
	}
	return &huma.RequestBody{
		Description: "A request in the path's wire shape. Relay reads model and stream; every other field goes to the upstream, translated when it speaks another shape.",
		Required:    true,
		Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{
			Type:                 huma.TypeObject,
			Required:             []string{"model"},
			Properties:           props,
			AdditionalProperties: true,
		}}},
	}
}

// describeListingErrors replaces bare status texts on the read-only routes' error responses.
func describeListingErrors(api huma.API) {
	for _, item := range api.OpenAPI().Paths {
		op := item.Get
		if op == nil {
			continue
		}
		for code, resp := range op.Responses {
			status, err := strconv.Atoi(code)
			if err != nil || resp.Description != http.StatusText(status) {
				continue
			}
			if d, ok := listingErrors[status]; ok {
				resp.Description = d
			}
		}
	}
}
