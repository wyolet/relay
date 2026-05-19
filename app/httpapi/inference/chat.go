package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
)

func registerChat(api huma.API, d Deps, mw huma.Middlewares) {
	registerChatAt(api, d, mw, "/v1/chat/completions", "chat_completions",
		"Create a chat completion (OpenAI-compatible)")
	registerChatAt(api, d, mw, "/openai/v1/chat/completions", "openai_chat_completions",
		"Create a chat completion via the explicit /openai namespace")
}

// registerChatAt registers one POST endpoint that delegates to handleChat.
// The path is parameterised so we can expose the same flow at the legacy
// /v1/* path and the namespaced /openai/v1/* path simultaneously.
func registerChatAt(api huma.API, d Deps, mw huma.Middlewares, path, opID, summary string) {
	huma.Register(api, huma.Operation{
		OperationID: opID,
		Method:      http.MethodPost,
		Path:        path,
		Summary:     summary,
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		// Huma doesn't surface the *http.Request inside the Operation
		// handler; we read it from the response stream callback below.
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			handleChat(d, w, r)
		}}, nil
	})
}

// handleChat is the per-request flow: parse → resolve → pipeline.Run →
// stream upstream response back to the caller.
func handleChat(d Deps, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	cls := ClassificationFrom(ctx)
	if cls.Mode == ModeProxyAuthed || cls.Mode == ModeProxyAnonymous {
		handleProxy(d, w, r, adapters.OpenAI)
		return
	}

	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		// Auth middleware should have stopped us; defensive.
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	cr, err := pkgopenai.Parse(ctx, body, r.Header)
	if err != nil {
		if status, b, ok := pkgopenai.ParseError(err); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}

	plan, err := d.Resolver.Resolve(routing.Request{
		ModelName: cr.Model,
		RelayKey:  rk,
	})
	if err != nil {
		mapRoutingErr(w, err)
		return
	}

	// Adapter mismatch — caller hit /v1/chat/completions but the
	// model's binding declares adapter=anthropic. Reject; cross-shape
	// translation is intentionally not v1.
	if plan.HostBinding.Adapter != adapters.OpenAI {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "adapter_mismatch",
			"model is not served via the OpenAI shape on this host; use /v1/messages if applicable")
		return
	}

	ad, ok := d.Adapters[plan.HostBinding.Adapter]
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(plan.HostBinding.Adapter))
		return
	}

	preq := &pipeline.Request{
		Body:        body,
		Headers:     r.Header,
		HostBaseURL: plan.Host.Spec.BaseURL,
		Adapter:     ad,
		Policy:      plan.Policy,
		Model:       plan.Model,
		Host:        plan.Host,
		Provider:    plan.Provider,
		Keys:        plan.Keys,
		ModelName:   plan.Model.Meta.Name,
	}

	result, err := d.Pipeline.Run(ctx, preq)
	if err != nil {
		mapPipelineErr(w, err)
		return
	}
	defer result.Body.Close()

	// Stream response back. Copy upstream headers (Content-Type etc.)
	// but strip hop-by-hop ones we don't want to leak.
	for k, vs := range result.Headers {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(result.Status)
	_, _ = io.Copy(w, result.Body)
}

// mapRoutingErr translates a routing sentinel to a typed HTTP error.
func mapRoutingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, routing.ErrModelNotFound):
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
	case errors.Is(err, routing.ErrModelDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_disabled", "model is disabled")
	case errors.Is(err, routing.ErrPolicyNotFound):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_not_found", "policy not found")
	case errors.Is(err, routing.ErrPolicyDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_disabled", "policy is disabled")
	case errors.Is(err, routing.ErrPolicyless):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policyless_disabled", "this relay key has no policy attached; policy-less traffic is disabled on this relay")
	case errors.Is(err, routing.ErrModelNotInPolicy):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_not_allowed", "model is not allowed by this policy")
	case errors.Is(err, routing.ErrNoHostBinding):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_host_binding", "no enabled host binding for model")
	case errors.Is(err, routing.ErrHostNotFound):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "host_not_found", "host referenced by binding not found")
	case errors.Is(err, routing.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys available")
	default:
		writeAPIError(w, http.StatusInternalServerError, "server_error", "routing_error", err.Error())
	}
}

// mapPipelineErr translates pipeline sentinels to HTTP responses.
func mapPipelineErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pipeline.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys")
	case errors.Is(err, pipeline.ErrAllKeysExhausted):
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unavailable", "all upstream keys failed")
	case errors.Is(err, pipeline.ErrAdapterMissing):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter", "adapter missing")
	default:
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", err.Error())
	}
}

// writeAPIError emits an OpenAI-shape error envelope.
func writeAPIError(w http.ResponseWriter, status int, errType, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	env := httpapi.OpenAIError{
		Err:        httpapi.OpenAIErrorInner{Type: errType, Code: code, Message: msg},
		HTTPStatus: status,
	}
	body, _ := json.Marshal(env)
	_, _ = w.Write(body)
}

// isHopByHop returns true for headers that mustn't traverse the proxy.
func isHopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailers", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

