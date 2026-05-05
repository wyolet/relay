package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
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

		var generic map[string]json.RawMessage
		if err := json.Unmarshal(body, &generic); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON", "")
			return
		}

		modelRaw, ok := generic["model"]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required", "")
			return
		}
		var modelName string
		if err := json.Unmarshal(modelRaw, &modelName); err != nil || modelName == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "model must be a non-empty string", "")
			return
		}

		plan, ok := resolve(modelName)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("model %q not found", modelName), "model_not_found")
			return
		}

		upstream := plan.Model.Spec.UpstreamName
		forwardBody := body
		if upstream != modelName {
			generic["model"], _ = json.Marshal(upstream)
			forwardBody, _ = json.Marshal(generic)
		}

		labels, err := parseMetadata(r.Header.Get("X-Relay-Metadata"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "invalid_metadata")
			return
		}

		msg := &transport.Message{
			ID:          reqid.From(r.Context()),
			ParentID:    "",
			Body:        forwardBody,
			Headers:     map[string]string{"Content-Type": r.Header.Get("Content-Type")},
			Labels:      labels,
			ReceivedAt:  time.Now().UTC(),
		}

		ch := transport.NewChannel(r.Context(), msg.ID, 1, 64)
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
			log.Printf("pipeline: %v", err)
		}
	}
}

var errMetadataTooManyKeys = errors.New("X-Relay-Metadata: too many keys (max 16)")

func parseMetadata(headerValue string) (map[string]string, error) {
	if headerValue == "" {
		return nil, nil
	}
	pairs := strings.Split(headerValue, ",")
	if len(pairs) > 16 {
		return nil, errMetadataTooManyKeys
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			return nil, fmt.Errorf("X-Relay-Metadata: malformed entry %q (expected k=v)", pair)
		}
		k := pair[:idx]
		v := pair[idx+1:]
		if len(k) > 128 {
			return nil, fmt.Errorf("X-Relay-Metadata: key too long (max 128 chars)")
		}
		if len(v) > 512 {
			return nil, fmt.Errorf("X-Relay-Metadata: value too long (max 512 chars)")
		}
		out[k] = v
	}
	return out, nil
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
