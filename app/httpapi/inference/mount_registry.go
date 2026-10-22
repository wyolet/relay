package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpmw"
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
				mountProfileAliases(api, d, mw, spec, path)
			}
		}
	}
}

// mountProfileAliases mirrors one inbound path under /{profile}/ for every
// registered profile speaking that spec's shape — a profile speaking
// several gets the prefix mirrored over each of them, so a client that
// switches wire shapes keeps the same base URL. The routes are
// hidden from the OpenAPI doc — they are the same operation, not a second
// one — and the profile is set on the request context explicitly rather
// than relying on the middleware's prefix resolution.
func mountProfileAliases(api huma.API, d Deps, mw huma.Middlewares, spec *adapter.Spec, path adapter.InboundPath) {
	shapeNamespace := clientprofile.FirstPathSegment(path.Path)
	for _, pr := range d.Profiles.Profiles() {
		if !clientprofile.Speaks(pr, string(spec.Name)) {
			continue
		}
		profile := pr
		huma.Register(api, huma.Operation{
			OperationID: path.OperationID + "_" + profile.Name(),
			Method:      http.MethodPost,
			Path:        "/" + profile.Name() + strings.TrimPrefix(path.Path, "/"+shapeNamespace),
			Summary:     path.Summary,
			Tags:        []string{"inference"},
			Middlewares: mw,
			Hidden:      true,
			Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
		}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(hctx huma.Context) {
				r, w := humachi.Unwrap(hctx)
				handleShape(spec, d, w, r.WithContext(clientprofile.WithProfile(r.Context(), profile)))
			}}, nil
		})
	}
}

// extractModelStream extracts the model name and stream flag from a raw JSON
// request body, spanning every inbound shape. Vendor shapes signal streaming
// with a top-level "stream" bool; the canonical shape uses
// "output_mode":"stream". The model field is read as a JSON string (the
// single-model case); canonical's array form is rejected upstream as multiplex
// and isn't used by streaming clients. Model may be absent (empty string
// returned); callers check. A body with more than one top-level model key is
// errDuplicateModel: encoding/json keeps the last, and the bytes a byte-pass
// forwards may be read differently upstream.
func extractModelStream(body []byte) (model string, stream bool, err error) {
	var probe struct {
		Model      string `json:"model"`
		Stream     *bool  `json:"stream"`
		OutputMode string `json:"output_mode"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false, fmt.Errorf("invalid JSON: %w", err)
	}
	var scan modelScanner
	if scan.feed(body); scan.err == errDuplicateModel {
		return "", false, errDuplicateModel
	}
	s := probe.OutputMode == "stream"
	if probe.Stream != nil {
		s = *probe.Stream
	}
	return probe.Model, s, nil
}

// handleShape is the generic per-shape route handler. It reads the request
// body, performs a minimal JSON parse to extract model and stream, and calls
// Dispatch with the spec's Name as the inbound shape.
func handleShape(spec *adapter.Spec, d Deps, w http.ResponseWriter, r *http.Request) {
	body, err := readRequestBody(r, d.MaxRequestBytes)
	if err != nil {
		if httpmw.IsBodyTooLargeError(err) {
			WriteAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", err.Error())
			return
		}
		WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	modelName, stream, err := extractModelStream(body)
	if err != nil {
		writeModelParseErr(w, err)
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

// writeModelParseErr answers an extractModelStream error.
func writeModelParseErr(w http.ResponseWriter, err error) {
	code := "parse_error"
	if errors.Is(err, errDuplicateModel) {
		code = errDuplicateModel.code
	}
	WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", code, err.Error())
}

// maxUpfrontBodyBytes caps what a declared Content-Length allocates before the bytes arrive, so a caller cannot reserve memory it never sends.
const maxUpfrontBodyBytes = 1 << 20

// readRequestBody sizes the buffer from the declared Content-Length when it is within limit: one exact allocation up to maxUpfrontBodyBytes, then growth capped at the declared length as bytes arrive, instead of io.ReadAll's growth to about twice the body. An unknown or larger length goes through io.ReadAll, where the listener's MaxBytesReader rejects an oversized body. On error the bytes read so far are returned with it.
func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = httpmw.DefaultMaxRequestBytes
	}
	declared := r.ContentLength
	if declared <= 0 || declared > limit {
		return io.ReadAll(r.Body)
	}
	body := make([]byte, 0, min(declared, maxUpfrontBodyBytes))
	for int64(len(body)) < declared {
		if len(body) == cap(body) {
			body = slices.Grow(body, int(min(declared, 2*int64(cap(body))))-len(body))
		}
		n, err := r.Body.Read(body[len(body):cap(body)])
		body = body[:len(body)+n]
		if err == io.EOF {
			if int64(len(body)) < declared {
				return body, io.ErrUnexpectedEOF
			}
			return body, nil
		}
		if err != nil {
			return body, err
		}
	}
	var next [1]byte
	n, err := io.ReadFull(r.Body, next[:])
	if n == 0 {
		if err == io.EOF {
			return body, nil
		}
		return body, err
	}
	// More bytes than declared: keep them, as io.ReadAll would.
	rest, err := io.ReadAll(r.Body)
	return append(append(body, next[0]), rest...), err
}
