package openai

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
	pkgopenai "github.com/wyolet/relay/pkg/api/openai"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
)

// RequestPlan is an alias for routing.RequestPlan so existing callers don't break.
// Canonical definition lives in internal/routing to keep the import graph cycle-free.
type RequestPlan = routing.RequestPlan

// Pipeline orchestrates message flow for one request through a Channel.
// It returns a RunResult carrying upstream duration alongside any error.
type Pipeline func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) (pipeline.RunResult, error)

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
			metricChatRequests.WithLabelValues(safeLabel(modelName), statusClass(statusCode)).Inc()
			metricChatDuration.WithLabelValues(safeLabel(modelName)).Observe(total.Seconds())
			if upstreamDur > 0 && upstreamDur < total {
				metricChatOverhead.WithLabelValues(safeLabel(modelName)).Observe((total - upstreamDur).Seconds())
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
			RouteHeader: r.Header.Get("X-Relay-Route"),
			ModelName:   cr.Model,
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
			default:
				statusCode = http.StatusInternalServerError
				writeError(w, statusCode, "api_error",
					resolveErr.Error(), "")
			}
			return
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

		// Forward the raw body to upstream; rewrite model field if upstream name differs.
		forwardBody := cr.Raw
		upstream := plan.Model.Spec.UpstreamName
		if upstream != cr.Model {
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
					// Mid-stream error: emit SSE error event then [DONE].
					w.Write([]byte("data: "))
					w.Write(outMsg.Body)
					w.Write([]byte("\n\ndata: [DONE]\n\n"))
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
