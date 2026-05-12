package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/internal/pipeline"
	"github.com/wyolet/relay/internal/routing"
	"github.com/wyolet/relay/internal/usage"
	pkgopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// RequestPlan is an alias for routing.RequestPlan so existing callers don't break.
// Canonical definition lives in internal/routing to keep the import graph cycle-free.
type RequestPlan = routing.RequestPlan

// Pipeline runs the pipeline for one request and returns a typed Response.
// All HTTP framing (status code, headers, body copy, SSE chunking) is the
// caller's responsibility; pipeline.Run is free of net/http concerns.
type Pipeline func(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error)

// ChatCompletions returns an http.HandlerFunc. resolver resolves the routing.Request
// to a RequestPlan; runPipeline orchestrates the message flow.
func ChatCompletions(resolver *routing.Resolver, runPipeline Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		statusCode := 200
		var modelName string
		var upstreamDur time.Duration
		defer func() {
			total := time.Since(start)
			metrics.RequestTotal.WithLabelValues("openai", metrics.SafeLabel(modelName), metrics.StatusClass(statusCode)).Inc()
			metrics.RequestDuration.WithLabelValues("openai", metrics.SafeLabel(modelName)).Observe(total.Seconds())
			if upstreamDur > 0 && upstreamDur < total {
				metrics.RequestOverhead.WithLabelValues("openai", metrics.SafeLabel(modelName)).Observe((total - upstreamDur).Seconds())
			}
		}()

		httpheader.StripInbound(r.Header)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			if httpmw.IsBodyTooLargeError(err) {
				statusCode = http.StatusRequestEntityTooLarge
				writeError(w, statusCode, "invalid_request_error",
					fmt.Sprintf("request body exceeds %d bytes", httpmw.DefaultMaxRequestBytes), "request_too_large")
				return
			}
			statusCode = http.StatusBadRequest
			writeError(w, statusCode, "invalid_request_error", "failed to read request body", "")
			return
		}

		cr, parseErr := pkgopenai.Parse(r.Context(), body, r.Header)
		if parseErr != nil {
			if status, pbody, ok := pkgopenai.ParseError(parseErr); ok {
				statusCode = status
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(pbody)
				return
			}
			statusCode = http.StatusBadRequest
			writeError(w, statusCode, "invalid_request_error", parseErr.Error(), "")
			return
		}

		// Model is known after a successful parse.
		modelName = cr.Model

		ctx := r.Context()
		ctx = pkgopenai.ContextWithChatRequest(ctx, cr)

		plan, resolveErr := resolver.Resolve(routing.Request{
			RouteHeader:    r.Header.Get("X-Relay-Route"),
			ModelName:      cr.Model,
			PolicyOverride: auth.SubjectFrom(r.Context()).PolicyRef,
		})
		if resolveErr != nil {
			switch {
			case errors.Is(resolveErr, routing.ErrUnknownRoute):
				statusCode = http.StatusNotFound
				writeError(w, statusCode, "invalid_request_error",
					resolveErr.Error(), "route_not_found")
			case errors.Is(resolveErr, routing.ErrModelNotInRoute):
				statusCode = http.StatusBadRequest
				writeError(w, statusCode, "invalid_request_error",
					resolveErr.Error(), "model_not_in_route")
			case errors.Is(resolveErr, routing.ErrUnknownModel):
				statusCode = http.StatusNotFound
				writeError(w, statusCode, "invalid_request_error",
					fmt.Sprintf("model %q not found", cr.Model), "model_not_found")
			case errors.Is(resolveErr, routing.ErrNoModelSpecified):
				statusCode = http.StatusBadRequest
				writeError(w, statusCode, "invalid_request_error",
					resolveErr.Error(), "model_not_specified")
			case errors.Is(resolveErr, routing.ErrModelNotAllowed):
				statusCode = http.StatusForbidden
				writeError(w, statusCode, "invalid_request_error",
					fmt.Sprintf("model %q not allowed by policy", cr.Model), "model_not_allowed")
			default:
				statusCode = http.StatusInternalServerError
				writeError(w, statusCode, "api_error",
					resolveErr.Error(), "")
			}
			return
		}

		// Passthrough is decided at auth time. When set, enforce the global
		// passthrough models allowlist before forwarding upstream.
		var passthroughAuth string
		if subj := auth.SubjectFrom(r.Context()); subj.PassthroughAuth != "" {
			pt := resolver.Passthrough()
			if !pt.AllowsModel(cr.Model) {
				statusCode = http.StatusForbidden
				writeError(w, statusCode, "invalid_request_error",
					fmt.Sprintf("model %q not allowed by passthrough config", cr.Model), "model_not_allowed")
				return
			}
			plan.Passthrough = true
			plan.PassthroughAuth = subj.PassthroughAuth
			passthroughAuth = subj.PassthroughAuth
		}

		// Build attribution: header wins over body metadata (M4 contract preserved).
		var attribution map[string]string
		if hv := r.Header.Get("X-Relay-Metadata"); hv != "" {
			attribution = usage.ParseMetadataHeader(hv)
		} else if cr.Metadata != nil {
			attribution = cr.Metadata
		} else {
			attribution = reqid.Attribution(ctx)
		}

		// Forward the raw body verbatim; consumer's model name is authoritative.
		req := &pipeline.Request{
			Body:            cr.Raw,
			Attribution:     attribution,
			PassthroughAuth: passthroughAuth,
			Provider:        plan.Provider,
			Policy:          plan.Policy,
			Model:           plan.Model,
			Secrets:         plan.Secrets,
			Rules:           plan.Rules,
		}

		resp, pipeErr := runPipeline(ctx, req)
		if pipeErr != nil && resp == nil {
			// Pipeline failed before producing any response (e.g. context cancelled
			// before first message). Log and return 500 if headers not yet sent.
			reqid.Logger(r.Context()).Warn("pipeline error", "err", pipeErr)
			statusCode = http.StatusInternalServerError
			writeError(w, statusCode, "api_error", "internal error", "")
			return
		}
		if resp == nil {
			statusCode = http.StatusInternalServerError
			writeError(w, statusCode, "api_error", "internal error", "")
			return
		}

		statusCode = resp.Status
		ct := resp.Headers["Content-Type"]
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if ra := resp.Headers["Retry-After"]; ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(resp.Status)

		isStreaming := strings.HasPrefix(ct, "text/event-stream")
		if isStreaming {
			writeSSE(w, resp.Body)
		} else {
			io.Copy(w, resp.Body)
		}

		if pipeErr != nil {
			reqid.Logger(r.Context()).Warn("pipeline error", "err", pipeErr)
		}
		upstreamDur = resp.UpstreamDuration
	}
}

// writeSSE copies body to w, flushing after each read. It does not add SSE
// framing — the upstream bytes are already in SSE format (data: ...\n\n).
func writeSSE(w http.ResponseWriter, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

type errEnvelope struct {
	Error errBody `json:"error"`
}

type errBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, errType, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errEnvelope{
		Error: errBody{Message: msg, Type: errType, Code: code},
	})
}
