package anthropic

import (
	"context"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi/inference"
	pkganthropic "github.com/wyolet/relay/pkg/adapters/anthropic"
)

// MountRoutes registers the Anthropic-shape inbound endpoints. Per Path
// A, bare `/v1/messages` also serves Anthropic shape during the
// transition.
func MountRoutes(api huma.API, d inference.Deps, mw huma.Middlewares) {
	mountAt(api, d, mw, "/v1/messages", "messages",
		"Create a message (Anthropic-compatible)")
	mountAt(api, d, mw, "/anthropic/v1/messages", "anthropic_messages",
		"Create a message via the explicit /anthropic namespace")
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

// handle does the Anthropic-specific minimal parse and hands off to the
// shape-agnostic Dispatch.
func handle(d inference.Deps, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	req, err := pkganthropic.Parse(body)
	if err != nil {
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}

	inference.Dispatch(d, w, r, inference.DispatchInput{
		Inbound:   adapters.Anthropic,
		Body:      body,
		ModelName: req.Model,
		Stream:    req.Stream,
	})
}
