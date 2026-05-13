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
	"reflect"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// Version is the human-facing build version surfaced in the OpenAPI Info
// block and the /version endpoint. Bumped manually for now; later wired to
// `git describe` via -ldflags.
const Version = "0.1.0"

// installOnce gates the global huma overrides (error rewriter + schema
// namer) so both planes' Mount() can call Install without doubling up.
var installOnce sync.Once

// Install installs the process-global huma overrides: OpenAI-compatible
// error envelope, and a schema namer that prefixes type names with their
// package's last segment (e.g. provider_Spec vs host_Spec) so the catalog
// kinds' uniform Spec sub-structs don't collide in the OpenAPI schema
// registry.
//
// Idempotent — safe to call from every Mount entrypoint.
func Install() {
	installOnce.Do(func() {
		installErrorRewriter()
		installSchemaNamer()
	})
}

// InstallErrorRewriter is the legacy alias for Install(). Retained until
// callers migrate.
func InstallErrorRewriter() { Install() }

func installErrorRewriter() {
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

// installSchemaNamer is a no-op kept for symmetry; per-plane registries
// are wired via NewRegistry() below because huma.DefaultSchemaNamer is a
// function (not an assignable variable) in v2.37+.
func installSchemaNamer() {}

// NewRegistry returns a huma schema Registry whose namer produces
// clean PascalCase schema ids in the generated OpenAPI:
//
//   - The entity types Provider / Host / Model / HostKey / RateLimit /
//     Policy / Pricing / RelayKey keep their bare names.
//   - Sub-types defined inside an entity package get the entity name
//     prepended so the 8 colliding `Spec` types become `ProviderSpec`,
//     `HostSpec`, `ModelSpec`, etc. without renaming the Go types.
//   - Types in non-entity packages (meta.Metadata, meta.Owner,
//     adapter.Kind, …) keep their bare names — they don't collide.
//
// Plane Mount() functions install the returned registry on their
// huma.Config.
func NewRegistry() huma.Registry {
	return huma.NewMapRegistry("#/components/schemas/", schemaNamer)
}

// entityNameByPkg maps each app/<entity> package path to the
// PascalCase name of its top-level entity type. Sub-types in these
// packages get the entity name prepended in OpenAPI schema ids; the
// entity type itself stays bare. Update this when adding a new
// catalog entity package.
var entityNameByPkg = map[string]string{
	"github.com/wyolet/relay/app/provider":  "Provider",
	"github.com/wyolet/relay/app/host":      "Host",
	"github.com/wyolet/relay/app/model":     "Model",
	"github.com/wyolet/relay/app/hostkey":   "HostKey",
	"github.com/wyolet/relay/app/ratelimit": "RateLimit",
	"github.com/wyolet/relay/app/policy":    "Policy",
	"github.com/wyolet/relay/app/pricing":   "Pricing",
	"github.com/wyolet/relay/app/relaykey":  "RelayKey",
}

func schemaNamer(t reflect.Type, hint string) string {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		return hint
	}
	if entity, ok := entityNameByPkg[t.PkgPath()]; ok && name != entity {
		// Sub-type in an entity package — prepend the entity name to
		// avoid collisions (every entity has a `Spec`, `ratelimit.Rule`
		// vs `pricing.Rate`, etc.).
		name = entity + name
	}
	return sanitizeSchemaName(name)
}

// sanitizeSchemaName collapses characters that are valid in Go type names
// (dots, brackets, asterisks from generic instantiations) into underscores
// so the resulting OpenAPI schema id is a clean identifier.
func sanitizeSchemaName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
