package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/pipeline"
	"github.com/wyolet/relay/internal/routing"
	"github.com/wyolet/relay/internal/usage"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
)

// RequestPlan is an alias for routing.RequestPlan so callers don't need an extra import.
type RequestPlan = routing.RequestPlan

// Pipeline orchestrates message flow for one request through a Channel.
type Pipeline func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) (pipeline.RunResult, error)

// MessagesHandler returns an http.HandlerFunc for POST /v1/messages.
func MessagesHandler(resolver *routing.Resolver, runPipeline Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		statusCode := 200
		var modelName string
		var upstreamDur time.Duration
		defer func() {
			total := time.Since(start)
			metricMessagesRequests.WithLabelValues(safeLabel(modelName), statusClass(statusCode)).Inc()
			metricMessagesDuration.WithLabelValues(safeLabel(modelName)).Observe(total.Seconds())
			if upstreamDur > 0 && upstreamDur < total {
				metricMessagesOverhead.WithLabelValues(safeLabel(modelName)).Observe((total - upstreamDur).Seconds())
			}
		}()

		// Capture passthrough auth and extra headers before StripInbound removes them.
		// These are forwarded verbatim to upstream when pool.passthrough=true.
		inboundAuth := r.Header.Get("Authorization")
		inboundPassthroughHeaders := capturePassthroughHeaders(r.Header)

		httpheader.StripInbound(r.Header)

		body, err := io.ReadAll(r.Body)
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

		mr, parseErr := Parse(body)
		if parseErr != nil {
			if status, pbody, ok := ParseError(parseErr); ok {
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

		plan, resolveErr := resolver.Resolve(routing.Request{
			RouteHeader: r.Header.Get("X-Relay-Route"),
			ModelName:   mr.Model,
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
			default:
				statusCode = http.StatusInternalServerError
				writeAnthropicError(w, statusCode, "api_error", resolveErr.Error())
			}
			return
		}

		// Stamp passthrough auth on the plan so the pipeline closure can use it.
		if plan.Passthrough {
			plan.PassthroughAuth = inboundAuth
			plan.PassthroughHeaders = inboundPassthroughHeaders
		}
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

		// Forward the raw body to upstream; rewrite model field if upstream name differs.
		forwardBody := mr.Raw
		upstream := plan.Model.Spec.UpstreamName
		if upstream != mr.Model {
			var generic map[string]json.RawMessage
			if err := json.Unmarshal(body, &generic); err == nil {
				generic["model"], _ = json.Marshal(upstream)
				forwardBody, _ = json.Marshal(generic)
			}
		}

		msg := &transport.Message{
			ID:          reqid.From(ctx),
			ParentID:    "",
			Body:        forwardBody,
			Headers:     map[string]string{"Content-Type": r.Header.Get("Content-Type")},
			Attribution: attribution,
			ReceivedAt:  time.Now().UTC(),
		}

		ch := transport.NewChannel(ctx, msg.ID, 1, 64)
		defer ch.Cancel()

		ch.In <- msg
		close(ch.In)

		type pipelineResult struct {
			res pipeline.RunResult
			err error
		}
		pipeResultCh := make(chan pipelineResult, 1)
		go func() {
			res, err := runPipeline(ch.Ctx, ch, plan)
			pipeResultCh <- pipelineResult{res: res, err: err}
		}()

		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		firstSeen := false
		isStreaming := false
		for {
			select {
			case <-r.Context().Done():
				goto done
			case outMsg, ok := <-ch.Out:
				if !ok {
					goto done
				}
				if !firstSeen {
					firstSeen = true
					status := 200
					if s := outMsg.Headers["X-Relay-Status"]; s != "" {
						if code, err := strconv.Atoi(s); err == nil {
							status = code
						}
					}
					statusCode = status
					ct := outMsg.Headers["Content-Type"]
					if ct != "" {
						w.Header().Set("Content-Type", ct)
					}
					isStreaming = strings.HasPrefix(ct, "text/event-stream")
					w.WriteHeader(status)
					if len(outMsg.Body) > 0 {
						w.Write(outMsg.Body)
						flush()
					}
					continue
				}
				// Subsequent messages.
				if isStreaming && outMsg.Headers["X-Relay-Final"] == "true" && len(outMsg.Body) > 0 {
					// Mid-stream error: emit the error body then done.
					w.Write(outMsg.Body)
					flush()
					goto done
				}
				if len(outMsg.Body) > 0 {
					w.Write(outMsg.Body)
					flush()
				}
			}
		}
	done:

		pr := <-pipeResultCh
		upstreamDur = pr.res.UpstreamDuration
		if pr.err != nil {
			reqid.Logger(r.Context()).Warn("pipeline error", "err", pr.err)
		}
	}
}

// capturePassthroughHeaders returns a copy of the headers that are in
// OutboundPassthroughExtra, for forwarding on passthrough pool requests.
func capturePassthroughHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for name, vals := range h {
		if httpheader.Match(name, httpheader.OutboundPassthroughExtra) && len(vals) > 0 {
			out[name] = vals[0]
		}
	}
	return out
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
