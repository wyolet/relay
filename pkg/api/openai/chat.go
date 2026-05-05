package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// ModelResolver looks up a model name from the request and returns
// the upstream-facing name to send to the provider. ok=false means
// "unknown model".
type ModelResolver func(name string) (upstreamName string, ok bool)

// ForwardFn forwards a chat-completion body to the upstream and writes
// the response into w.
type ForwardFn func(ctx context.Context, body []byte, w http.ResponseWriter) error

// ChatCompletions handles POST /v1/chat/completions in OpenAI shape.
// Validates the model against the resolver, rewrites it to the
// upstream-facing name when aliased, then forwards.
func ChatCompletions(resolve ModelResolver, forward ForwardFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body", "")
			return
		}

		// Parse generically so unknown fields (new OpenAI options,
		// vendor extensions) survive untouched.
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

		upstream, ok := resolve(modelName)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("model %q not found", modelName), "model_not_found")
			return
		}

		forwardBody := body
		if upstream != modelName {
			generic["model"], _ = json.Marshal(upstream)
			forwardBody, _ = json.Marshal(generic)
		}

		if err := forward(r.Context(), forwardBody, w); err != nil {
			log.Printf("forward: %v", err)
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
