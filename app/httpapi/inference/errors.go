package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
)

// WriteAPIError emits an OpenAI-shape error envelope. Exported so
// per-shape route packages (app/adapters/<name>/routes.go) can use it
// without depending on shape-specific helpers. Extra slog attrs are
// attached to the warning log only — never to the client envelope.
func WriteAPIError(w http.ResponseWriter, status int, errType, code, msg string, attrs ...any) {
	writeAPIError(w, status, errType, code, msg, attrs...)
}

// writeAPIError is the internal form used by handlers inside this
// package; WriteAPIError is the exported wrapper for adapter packages.
// attrs add structured fields to the log line (e.g. the requested model
// and policy on a routing rejection) without leaking them into the
// client-facing error body.
func writeAPIError(w http.ResponseWriter, status int, errType, code, msg string, attrs ...any) {
	slog.Warn("inference: error response",
		append([]any{"status", status, "type", errType, "code", code, "msg", msg}, attrs...)...,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	env := httpapi.OpenAIError{
		Err:        httpapi.OpenAIErrorInner{Type: errType, Code: code, Message: msg},
		HTTPStatus: status,
	}
	body, _ := json.Marshal(env)
	_, _ = w.Write(body)
}

// mapRoutingErr translates a routing sentinel to a typed HTTP error. model is
// the caller-supplied model ref (safe to echo back); policy is the resolved
// policy id (logged only, never returned to the caller). Either may be "".
func mapRoutingErr(w http.ResponseWriter, err error, model, policy string) {
	// log-only attrs; the requested model + policy make a routing rejection
	// diagnosable from logs without re-deriving them from the request.
	attrs := []any{"model", model, "policy", policy}
	// modelMsg echoes the requested model into the client message when known.
	modelMsg := func(format, fallback string) string {
		if model == "" {
			return fallback
		}
		return fmt.Sprintf(format, model)
	}
	switch {
	case errors.Is(err, routing.ErrModelNotFound):
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", modelMsg("model %q not found", "model not found"), attrs...)
	case errors.Is(err, routing.ErrModelDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_disabled", modelMsg("model %q is disabled", "model is disabled"), attrs...)
	case errors.Is(err, routing.ErrPolicyNotFound):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_not_found", "policy not found", attrs...)
	case errors.Is(err, routing.ErrPolicyDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_disabled", "policy is disabled", attrs...)
	case errors.Is(err, routing.ErrPolicyless):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policyless_disabled", "this relay key has no policy attached; policy-less traffic is disabled on this relay", attrs...)
	case errors.Is(err, routing.ErrModelNotInPolicy):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_not_allowed", modelMsg("model %q is not allowed by this policy", "model is not allowed by this policy"), attrs...)
	case errors.Is(err, routing.ErrNoHostBinding):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_host_binding", "no enabled host binding for model", attrs...)
	case errors.Is(err, routing.ErrHostNotFound):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "host_not_found", "host referenced by binding not found", attrs...)
	case errors.Is(err, routing.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys available", attrs...)
	default:
		writeAPIError(w, http.StatusInternalServerError, "server_error", "routing_error", err.Error(), attrs...)
	}
}

// routingErrKind maps a routing sentinel to a machine-readable usage
// ErrorKind. Mirrors mapRoutingErr's switch so the usage log and the HTTP
// response agree on what was rejected. The event Status stays 0 (upstream
// never reached); the kind carries the reason.
func routingErrKind(err error) string {
	switch {
	case errors.Is(err, routing.ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, routing.ErrModelDisabled):
		return "model_disabled"
	case errors.Is(err, routing.ErrPolicyNotFound):
		return "policy_not_found"
	case errors.Is(err, routing.ErrPolicyDisabled):
		return "policy_disabled"
	case errors.Is(err, routing.ErrPolicyless):
		return "policyless"
	case errors.Is(err, routing.ErrModelNotInPolicy):
		return "model_not_allowed"
	case errors.Is(err, routing.ErrNoHostBinding):
		return "no_host_binding"
	case errors.Is(err, routing.ErrHostNotFound):
		return "host_not_found"
	case errors.Is(err, routing.ErrNoKeys):
		return "no_keys"
	default:
		return "routing_error"
	}
}

// mapPipelineErr translates pipeline sentinels to HTTP responses.
func mapPipelineErr(w http.ResponseWriter, err error) {
	var upstream *pipeline.UpstreamFailureError
	var unreachable *pipeline.UpstreamUnreachableError
	switch {
	case errors.As(err, &unreachable):
		// Dial failure against the host — likely a misconfigured baseURL or a
		// down upstream, not a key problem. Surface it distinctly so operators
		// don't chase key/pool config.
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unreachable", unreachable.Error())
	case errors.Is(err, pipeline.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys")
	case errors.As(err, &upstream):
		// Surface the upstream's actual status + body so callers see auth /
		// quota / bad-model messages instead of a generic "keys exhausted".
		msg := "all upstream keys failed; " + upstream.Error()
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unavailable", msg)
	case errors.Is(err, pipeline.ErrAllKeysExhausted):
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unavailable", "all upstream keys failed")
	case errors.Is(err, pipeline.ErrAdapterMissing):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter", "adapter missing")
	default:
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", err.Error())
	}
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

// ForwardUpstreamHeaders copies src → dst, dropping hop-by-hop. The caller
// is responsible for any further adjustments (e.g. clearing Content-Length
// when the body size will change between upstream and client). Exported so
// adapter packages can use it from their own cross-shape handlers.
func ForwardUpstreamHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// MapPipelineErr is the exported form for adapter-side cross-shape handlers
// that drive pipeline.Pipeline.Run directly.
func MapPipelineErr(w http.ResponseWriter, err error) { mapPipelineErr(w, err) }

// SplitSSEChunks is exported so adapter packages can use the same SSE
// chunking logic in their cross-shape stream handlers.
func SplitSSEChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	return splitSSEChunks(data, atEOF)
}
