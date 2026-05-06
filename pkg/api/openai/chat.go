package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
	"github.com/wyolet/relay/pkg/usage"
)

// RequestPlan holds the resolved model, provider, pool, secrets, and rate-limit
// rules for a request. Rules are pre-resolved for Pool+Model scope at plan time;
// Secret-level rules are M4+ work.
type RequestPlan struct {
	Model    *configstore.Model
	Provider *configstore.Provider
	Pool     *configstore.Pool
	Secrets  []*configstore.Secret
	Rules    []configstore.ResolvedRule
}

// PlanResolver resolves a model name to a RequestPlan. ok=false means unknown model.
type PlanResolver func(modelName string) (*RequestPlan, bool)

// Pipeline orchestrates message flow for one request through a Channel.
type Pipeline func(ctx context.Context, ch *transport.Channel, plan *RequestPlan) error

// ChatCompletions returns an http.HandlerFunc. resolve builds the RequestPlan;
// runPipeline orchestrates the message flow.
func ChatCompletions(resolve PlanResolver, runPipeline Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpheader.StripInbound(r.Header)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			if httpmw.IsBodyTooLargeError(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
					fmt.Sprintf("request body exceeds %d bytes", httpmw.DefaultMaxRequestBytes), "request_too_large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body", "")
			return
		}

		cr, parseErr := Parse(r.Context(), body, r.Header)
		if parseErr != nil {
			if status, pbody, ok := ParseError(parseErr); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(pbody)
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request_error", parseErr.Error(), "")
			return
		}

		ctx := r.Context()
		ctx = ContextWithChatRequest(ctx, cr)

		plan, ok := resolve(cr.Model)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("model %q not found", cr.Model), "model_not_found")
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

		pipeErr := make(chan error, 1)
		go func() {
			pipeErr <- runPipeline(ch.Ctx, ch, plan)
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

		if err := <-pipeErr; err != nil {
			reqid.Logger(r.Context()).Warn("pipeline error", "err", err)
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
