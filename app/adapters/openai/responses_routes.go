package openai

import (
	"context"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi/inference"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
)

// MountResponsesRoutes registers the OpenAI Responses API inbound endpoint.
// Phase 1: byte-passthrough to OpenAI's /v1/responses; non-OpenAI hosts
// are rejected with 400 at the Dispatch layer.
func MountResponsesRoutes(api huma.API, d inference.Deps, mw huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "openai_responses_create",
		Method:      http.MethodPost,
		Path:        "/openai/v1/responses",
		Summary:     "Create a response (OpenAI Responses API)",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			handleResponses(d, w, r)
		}}, nil
	})
}

func handleResponses(d inference.Deps, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	cr, err := pkgopenai.Parse(r.Context(), body, r.Header)
	if err != nil {
		if status, b, ok := pkgopenai.ParseError(err); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(b)
			return
		}
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}

	inference.Dispatch(d, w, r, inference.DispatchInput{
		Inbound:   adapters.OpenAIResponses,
		Body:      body,
		ModelName: cr.Model,
		Stream:    cr.Stream,
	})
}
