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

// MountRoutes registers the OpenAI-shape inbound endpoints. Per Path A,
// the bare `/v1/chat/completions` also serves OpenAI shape during the
// transition — it migrates to the canonical shape when canonical lands.
func MountRoutes(api huma.API, d inference.Deps, mw huma.Middlewares) {
	mountAt(api, d, mw, "/v1/chat/completions", "chat_completions",
		"Create a chat completion (OpenAI-compatible)")
	mountAt(api, d, mw, "/openai/v1/chat/completions", "openai_chat_completions",
		"Create a chat completion via the explicit /openai namespace")
}

func mountAt(api huma.API, d inference.Deps, mw huma.Middlewares, path, opID, summary string) {
	huma.Register(api, huma.Operation{
		OperationID: opID,
		Method:      http.MethodPost,
		Path:        path,
		Summary:     summary,
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			handle(d, w, r)
		}}, nil
	})
}

// handle does the OpenAI-specific minimal parse and hands off to the
// shape-agnostic Dispatch.
func handle(d inference.Deps, w http.ResponseWriter, r *http.Request) {
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
		Inbound:   adapters.OpenAI,
		Body:      body,
		ModelName: cr.Model,
		Stream:    cr.Stream,
	})
}
