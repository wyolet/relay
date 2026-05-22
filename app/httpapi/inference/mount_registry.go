package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapter"
)

// MountRegistry returns a RouteMounter that registers one POST route per
// InboundPath across all specs in reg. Each route performs a minimal JSON
// parse (model + stream), then calls Dispatch with the spec's Name as the
// inbound shape.
//
// This is the generic route mounter that replaces per-shape MountRoutes
// functions. The composition root calls it once with the populated registry.
func MountRegistry(reg *adapter.Registry) RouteMounter {
	return func(api huma.API, d Deps, mw huma.Middlewares) {
		for _, s := range reg.Specs() {
			for _, ip := range s.InboundPaths {
				// Capture loop vars.
				spec := s
				path := ip
				huma.Register(api, huma.Operation{
					OperationID: path.OperationID,
					Method:      http.MethodPost,
					Path:        path.Path,
					Summary:     path.Summary,
					Tags:        []string{"inference"},
					Middlewares: mw,
					Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
				}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
					return &huma.StreamResponse{Body: func(hctx huma.Context) {
						r, w := humachi.Unwrap(hctx)
						handleShape(spec, d, w, r)
					}}, nil
				})
			}
		}
	}
}

// extractModelStream extracts the top-level "model" string and "stream" bool
// from a raw JSON request body. Returns an error if the body is not valid JSON.
// The model field may be absent (empty string returned); callers check.
func extractModelStream(body []byte) (model string, stream bool, err error) {
	var probe struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false, fmt.Errorf("invalid JSON: %w", err)
	}
	s := false
	if probe.Stream != nil {
		s = *probe.Stream
	}
	return probe.Model, s, nil
}

// handleShape is the generic per-shape route handler. It reads the request
// body, performs a minimal JSON parse to extract model and stream, and calls
// Dispatch with the spec's Name as the inbound shape.
func handleShape(spec *adapter.Spec, d Deps, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	modelName, stream, err := extractModelStream(body)
	if err != nil {
		WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}
	if modelName == "" {
		WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model",
			"field 'model' is required")
		return
	}

	// Byte-pass shapes (e.g. Embeddings) are never streaming.
	if spec.BytePass {
		stream = false
	}

	Dispatch(d, w, r, DispatchInput{
		Inbound:   spec.Name,
		Body:      body,
		ModelName: modelName,
		Stream:    stream,
	})
}
