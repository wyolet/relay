package anthropic

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
	pkganthropic "github.com/wyolet/relay/pkg/api/anthropic"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// RequestPlan is an alias for routing.RequestPlan so callers don't need an extra import.
type RequestPlan = routing.RequestPlan

// Pipeline runs the pipeline for one request and returns a typed Response.
// All HTTP framing is the caller's responsibility; pipeline.Run is free of net/http.
type Pipeline func(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error)

// MessagesHandler returns an http.HandlerFunc for POST /v1/messages.
func MessagesHandler(resolver *routing.Resolver, runPipeline Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		statusCode := 200
		var modelName string
		var upstreamDur time.Duration
		defer func() {
			total := time.Since(start)
			metrics.RequestTotal.WithLabelValues("anthropic", metrics.SafeLabel(modelName), metrics.StatusClass(statusCode)).Inc()
			metrics.RequestDuration.WithLabelValues("anthropic", metrics.SafeLabel(modelName)).Observe(total.Seconds())
			if upstreamDur > 0 && upstreamDur < total {
				metrics.RequestOverhead.WithLabelValues("anthropic", metrics.SafeLabel(modelName)).Observe((total - upstreamDur).Seconds())
			}
		}()

		// Capture passthrough auth and extra headers before StripInbound removes them.
		// These are forwarded verbatim to upstream when policy.passthrough=true.
		inboundAuth := r.Header.Get("Authorization")
		inboundPassthroughHeaders := capturePassthroughHeaders(r.Header)

		httpheader.StripInbound(r.Header)

		tStrip := time.Now()
		body, err := io.ReadAll(r.Body)
		tRead := time.Now()
		if err != nil {
			if httpmw.IsBodyTooLargeError(err) {
				statusCode = http.StatusRequestEntityTooLarge
				writeAnthropicError(w, statusCode, "invalid_request_error",
					fmt.Sprintf("request body exceeds %d bytes", httpmw.DefaultMaxRequestBytes))
				return
			}
			statusCode = http.StatusBadRequest
			writeAnthropicError(w, statusCode, "invalid_request_error", "failed to read request body")
			return
		}

		tParseStart := time.Now()
		mr, parseErr := pkganthropic.Parse(body)
		tParseEnd := time.Now()
		if parseErr != nil {
			if status, pbody, ok := pkganthropic.ParseError(parseErr); ok {
				statusCode = status
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(pbody)
				return
			}
			statusCode = http.StatusBadRequest
			writeAnthropicError(w, statusCode, "invalid_request_error", parseErr.Error())
			return
		}

		modelName = mr.Model

		ctx := r.Context()

		// TEMP instrumentation: log size + per-stage latency for big bodies.
		if len(body) > 1<<20 {
			reqid.Logger(ctx).Info("messages: big body",
				"bytes", len(body),
				"strip_to_read_ms", tRead.Sub(tStrip).Milliseconds(),
				"parse_ms", tParseEnd.Sub(tParseStart).Milliseconds(),
			)
		}

		plan, resolveErr := resolver.Resolve(routing.Request{
			RouteHeader:    r.Header.Get("X-Relay-Route"),
			ModelName:      mr.Model,
			PolicyOverride: auth.SubjectFrom(r.Context()).PolicyRef,
		})
		if resolveErr != nil {
			switch {
			case errors.Is(resolveErr, routing.ErrUnknownRoute):
				statusCode = http.StatusNotFound
				writeAnthropicError(w, statusCode, "not_found_error", resolveErr.Error())
			case errors.Is(resolveErr, routing.ErrModelNotInRoute):
				statusCode = http.StatusBadRequest
				writeAnthropicError(w, statusCode, "invalid_request_error", resolveErr.Error())
			case errors.Is(resolveErr, routing.ErrUnknownModel):
				statusCode = http.StatusNotFound
				writeAnthropicError(w, statusCode, "not_found_error", fmt.Sprintf("model %q not found", mr.Model))
			case errors.Is(resolveErr, routing.ErrNoModelSpecified):
				statusCode = http.StatusBadRequest
				writeAnthropicError(w, statusCode, "invalid_request_error", resolveErr.Error())
			case errors.Is(resolveErr, routing.ErrModelNotAllowed):
				statusCode = http.StatusForbidden
				writeAnthropicError(w, statusCode, "permission_error", fmt.Sprintf("model %q not allowed by policy", mr.Model))
			default:
				statusCode = http.StatusInternalServerError
				writeAnthropicError(w, statusCode, "api_error", resolveErr.Error())
			}
			return
		}

		// Passthrough is decided at auth time, not at routing time.
		var passthroughAuth string
		if subj := auth.SubjectFrom(r.Context()); subj.PassthroughAuth != "" {
			pt := resolver.Passthrough()
			if !pt.AllowsModel(mr.Model) {
				statusCode = http.StatusForbidden
				writeAnthropicError(w, statusCode, "permission_error", fmt.Sprintf("model %q not allowed by passthrough config", mr.Model))
				return
			}
			plan.Passthrough = true
			plan.PassthroughAuth = subj.PassthroughAuth
			plan.PassthroughHeaders = inboundPassthroughHeaders
			passthroughAuth = subj.PassthroughAuth
		}
		_ = inboundAuth
		plan.RawQuery = r.URL.RawQuery

		// Build attribution: header wins over body metadata.
		var attribution map[string]string
		if hv := r.Header.Get("X-Relay-Metadata"); hv != "" {
			attribution = usage.ParseMetadataHeader(hv)
		} else if mr.Metadata != nil {
			attribution = mr.Metadata
		} else {
			attribution = reqid.Attribution(ctx)
		}

		req := &pipeline.Request{
			Body:            mr.Raw,
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
			reqid.Logger(r.Context()).Warn("pipeline error", "err", pipeErr)
			statusCode = http.StatusInternalServerError
			writeAnthropicError(w, statusCode, "api_error", "internal error")
			return
		}
		if resp == nil {
			statusCode = http.StatusInternalServerError
			writeAnthropicError(w, statusCode, "api_error", "internal error")
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

// writeSSE copies body to w, flushing after each read.
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

// writeAnthropicError writes an Anthropic-shaped error response.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}
