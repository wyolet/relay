// Package httpapi holds the HTTP layer for both Relay planes.
//
//   - app/httpapi/inference  — data plane: /v1/*, /healthz
//   - app/httpapi/control    — admin plane: /auth/*, CRUD, /version, etc.
//
// Each subpackage exposes a typed Deps and a Mount(chi.Router, Deps) huma.API
// entrypoint. The top-level package owns shared concerns: the OpenAI-shape
// error envelope used by both planes, the huma↔chi middleware adapter, and
// the build/version string.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Version is the human-facing build version surfaced in the OpenAPI Info
// block and the /version endpoint. Bumped manually for now; later wired to
// `git describe` via -ldflags.
const Version = "0.1.0"

// InstallErrorRewriter overrides huma's default error model with an
// OpenAI-compatible envelope. Both planes share the same error shape so
// clients see one error contract regardless of which listener they hit.
//
// Idempotent — safe to call from multiple Mount() entrypoints.
func InstallErrorRewriter() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		code := ""
		errType := "invalid_request_error"
		for _, e := range errs {
			var mbe *http.MaxBytesError
			if errors.As(e, &mbe) {
				status = http.StatusRequestEntityTooLarge
				msg = "request body too large"
				break
			}
		}
		switch status {
		case http.StatusRequestEntityTooLarge:
			code = "request_too_large"
		case http.StatusUnprocessableEntity:
			code = "unprocessable_entity"
		case http.StatusTooManyRequests:
			errType = "rate_limit_exceeded"
			code = "rate_limit_exceeded"
			msg = "rate limit exceeded"
		case http.StatusInternalServerError:
			errType = "server_error"
			code = "internal_error"
		}
		return &OpenAIError{
			Err:        OpenAIErrorInner{Type: errType, Code: code, Message: msg},
			HTTPStatus: status,
		}
	}
}

// OpenAIErrorInner is the inner object of the OpenAI error envelope.
type OpenAIErrorInner struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// OpenAIError implements huma.StatusError with the OpenAI-compatible shape:
//
//	{ "error": { "type": "...", "code": "...", "message": "..." } }
type OpenAIError struct {
	Err        OpenAIErrorInner `json:"error"`
	HTTPStatus int              `json:"-"`
}

func (e *OpenAIError) GetStatus() int              { return e.HTTPStatus }
func (e *OpenAIError) Error() string               { return e.Err.Message }
func (e *OpenAIError) ContentType(_ string) string { return "application/json" }
