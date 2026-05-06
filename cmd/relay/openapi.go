package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

const relayVersion = "0.1.0"

func init() {
	// Override huma's default error model to produce OpenAI-compatible envelopes.
	// This ensures that huma-level errors (413 body too large, 422 validation, etc.)
	// arrive in the same shape as Relay's own errors.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		code := ""
		errType := "invalid_request_error"
		// Detect body-too-large regardless of the status huma assigns.
		// When httpmw.LimitBody (http.MaxBytesReader) triggers before huma's own
		// limit-reader, huma receives a *http.MaxBytesError and maps it to 500.
		// We promote that to 413 here.
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
		return &openAIError{
			Err:        openAIErrorInner{Type: errType, Code: code, Message: msg},
			httpStatus: status,
		}
	}
}

// openAIErrorInner is the inner object of the OpenAI error envelope.
type openAIErrorInner struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// openAIError implements huma.StatusError and outputs an OpenAI-compatible envelope.
// The Error field is exported so huma's SchemaLinkTransformer copies it into the
// wrapped struct (which adds $schema). The unexported httpStatus carries the HTTP code.
type openAIError struct {
	// Err is the inner OpenAI error object; serialized as "error" key.
	Err openAIErrorInner `json:"error"`

	httpStatus int
}

func (e *openAIError) GetStatus() int              { return e.httpStatus }
func (e *openAIError) Error() string               { return e.Err.Message }
func (e *openAIError) ContentType(_ string) string { return "application/json" }

// humaAuth converts a net/http middleware into a huma per-operation middleware.
// It is used to gate /v1/* and /admin/* endpoints with the same bearer-token
// check that was previously applied via chi Group.
func humaAuth(authMW func(http.Handler) http.Handler) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humachi.Unwrap(ctx)
		authMW(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			next(humachi.NewContext(ctx.Operation(), r2, w2))
		})).ServeHTTP(w, r)
	}
}

// adminHandlers holds the five http.HandlerFuncs produced by the crud factory for one kind.
type adminHandlers struct {
	list   http.HandlerFunc
	get    http.HandlerFunc
	create http.HandlerFunc
	update http.HandlerFunc
	del    http.HandlerFunc
}

// adminCRUD bundles all five kinds' handler sets for huma registration.
type adminCRUD struct {
	provider  adminHandlers
	pool      adminHandlers
	model     adminHandlers
	route     adminHandlers
	rateLimit adminHandlers
}

// mountHuma wraps chiRouter in a humachi-backed huma API and registers all
// public operations. Returns the huma API (used in tests to inspect the spec).
//
// Routing pattern: all business-logic handlers live as huma operations on the
// top-level chi router; auth is enforced per-operation via humaAuth (not via a
// chi Group). /openapi.json, /docs, /schemas are served unauthenticated by
// huma on the same router.
//
// adminH may be nil (admin not configured); its op is skipped in that case.
// crud may be nil (admin not configured); its ops are skipped in that case.
func mountHuma(
	chiRouter chi.Router,
	authMW func(http.Handler) http.Handler,
	healthzH http.HandlerFunc,
	chatH http.HandlerFunc,
	modelsH http.HandlerFunc,
	adminH http.HandlerFunc,
	crud *adminCRUD,
) huma.API {
	cfg := huma.DefaultConfig("Wyolet Relay", relayVersion)
	cfg.Info.Description = "High-throughput LLM router. " +
		"Chat-completions and models endpoints follow the OpenAI API shape " +
		"(https://platform.openai.com/docs/api-reference). " +
		"Only `model`, `stream`, and `user` are inspected by Relay; " +
		"all other fields are forwarded verbatim to the upstream provider."

	api := humachi.New(chiRouter, cfg)
	auth := huma.Middlewares{humaAuth(authMW)}

	// delegate wraps an http.HandlerFunc as a huma stream handler (no request body).
	delegate := func(h http.HandlerFunc) func(context.Context, *struct{}) (*huma.StreamResponse, error) {
		return func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{
				Body: func(ctx huma.Context) {
					r, w := humachi.Unwrap(ctx)
					h.ServeHTTP(w, r)
				},
			}, nil
		}
	}

	// delegateBody wraps an http.HandlerFunc as a huma stream handler with a raw body.
	// Huma reads r.Body to populate inp.RawBody for OpenAPI validation; we restore
	// r.Body from the parsed bytes so the downstream handler can re-read it.
	//
	// chatInput declares the documented fields so the generated OpenAPI spec exposes
	// them. Body handling is performed by the chat handler via Parse(); the parsed
	// values from huma (Model, Stream, User, Metadata) are intentionally discarded
	// here — they exist for documentation only.
	type chatInput struct {
		RawBody json.RawMessage `doc:"OpenAI-compatible chat completion request (see https://platform.openai.com/docs/api-reference/chat/create)."`

		// Documentation-only fields — Relay inspects these from the raw body via Parse().
		// Values set here by huma's decoder are never read.
		Model    string            `json:"model" doc:"ID of the model to use (required)."`
		Stream   bool              `json:"stream,omitempty" doc:"If true, partial message deltas are sent as SSE."`
		User     string            `json:"user,omitempty" doc:"Caller identifier forwarded to the upstream provider."`
		Metadata map[string]string `json:"metadata,omitempty" doc:"Up to 16 key/value pairs for caller attribution. Keys: [a-zA-Z0-9_.-], max 64 chars. Values: printable ASCII, max 256 chars."`
	}
	delegateBody := func(h http.HandlerFunc) func(context.Context, *chatInput) (*huma.StreamResponse, error) {
		return func(_ context.Context, inp *chatInput) (*huma.StreamResponse, error) {
			raw := inp.RawBody
			return &huma.StreamResponse{
				Body: func(ctx huma.Context) {
					r, w := humachi.Unwrap(ctx)
					// Restore the body that huma consumed during schema validation.
					r.Body = io.NopCloser(bytes.NewReader(raw))
					r.ContentLength = int64(len(raw))
					h.ServeHTTP(w, r)
				},
			}, nil
		}
	}

	// GET /healthz — open, no auth.
	huma.Register(api, huma.Operation{
		OperationID: "get-healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Health check",
		Description: "Returns overall status and per-backend health. HTTP 200 = ok, 503 = degraded.",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{
			Body: func(ctx huma.Context) {
				r, w := humachi.Unwrap(ctx)
				healthzH.ServeHTTP(w, r)
			},
		}, nil
	})

	// POST /v1/chat/completions — auth-gated.
	huma.Register(api, huma.Operation{
		OperationID: "create-chat-completion",
		Method:      http.MethodPost,
		Path:        "/v1/chat/completions",
		Summary:     "Create chat completion",
		Description: "Proxies to the configured upstream provider following the OpenAI Chat " +
			"Completions API shape (https://platform.openai.com/docs/api-reference/chat/create). " +
			"Returns text/event-stream when stream=true, application/json otherwise.",
		Tags:        []string{"chat"},
		Errors:      []int{400, 401, 404, 429, 500},
		Middlewares: auth,
	}, delegateBody(chatH))

	// Patch the generated OpenAPI spec for /v1/chat/completions: huma's RawBody
	// handling produces an opaque binary schema. We replace it with a proper
	// application/json schema that exposes the fields Relay inspects so that
	// API consumers (and scenario-7 smoke) can discover the metadata field.
	if op, ok := api.OpenAPI().Paths["/v1/chat/completions"]; ok {
		if op.Post != nil && op.Post.RequestBody != nil {
			op.Post.RequestBody.Content = map[string]*huma.MediaType{
				"application/json": {
					Schema: &huma.Schema{
						Type: "object",
						Properties: map[string]*huma.Schema{
							"model": {Type: "string", Description: "ID of the model to use (required)."},
							"stream": {Type: "boolean", Description: "If true, partial message deltas are sent as SSE."},
							"user": {Type: "string", Description: "Caller identifier forwarded to the upstream provider."},
							"metadata": {
								Type:        "object",
								Description: "Up to 16 key/value pairs for caller attribution. Keys: [a-zA-Z0-9_.-], max 64 chars. Values: printable ASCII, max 256 chars.",
								AdditionalProperties: &huma.Schema{Type: "string"},
							},
						},
						Required: []string{"model"},
					},
				},
			}
		}
	}

	// GET /v1/models — auth-gated.
	huma.Register(api, huma.Operation{
		OperationID: "list-models",
		Method:      http.MethodGet,
		Path:        "/v1/models",
		Summary:     "List models",
		Description: "Returns all models in Relay's catalog in OpenAI list shape.",
		Tags:        []string{"models"},
		Errors:      []int{401},
		Middlewares: auth,
	}, delegate(modelsH))

	// POST /admin/reload — auth-gated, conditional.
	if adminH != nil {
		huma.Register(api, huma.Operation{
			OperationID: "admin-reload",
			Method:      http.MethodPost,
			Path:        "/admin/reload",
			Summary:     "Reload catalog",
			Description: "Triggers a live config reload from the Postgres catalog. Requires admin bearer token.",
			Tags:        []string{"admin"},
			Errors:      []int{401, 429, 500},
			Middlewares: auth,
		}, delegate(adminH))
	}

	// Admin CRUD — five kinds × five verbs = 25 endpoints.
	if crud != nil {
		type kindSpec struct {
			singular string
			plural   string
			h        adminHandlers
		}
		kinds := []kindSpec{
			{"provider", "providers", crud.provider},
			{"pool", "pools", crud.pool},
			{"model", "models", crud.model},
			{"route", "routes", crud.route},
			{"ratelimit", "ratelimits", crud.rateLimit},
		}
		for _, k := range kinds {
			k := k
			base := "/admin/" + k.plural
			nameParam := base + "/{name}"

			huma.Register(api, huma.Operation{
				OperationID: "admin_" + k.singular + "_list",
				Method:      http.MethodGet,
				Path:        base,
				Summary:     "List " + k.plural,
				Tags:        []string{"admin"},
				Errors:      []int{500},
				Middlewares: auth,
			}, delegate(k.h.list))

			huma.Register(api, huma.Operation{
				OperationID: "admin_" + k.singular + "_get",
				Method:      http.MethodGet,
				Path:        nameParam,
				Summary:     "Get " + k.singular,
				Tags:        []string{"admin"},
				Errors:      []int{404, 500},
				Middlewares: auth,
			}, delegate(k.h.get))

			huma.Register(api, huma.Operation{
				OperationID: "admin_" + k.singular + "_create",
				Method:      http.MethodPost,
				Path:        base,
				Summary:     "Create " + k.singular,
				Tags:        []string{"admin"},
				Errors:      []int{400, 500},
				Middlewares: auth,
			}, delegate(k.h.create))

			huma.Register(api, huma.Operation{
				OperationID: "admin_" + k.singular + "_update",
				Method:      http.MethodPut,
				Path:        nameParam,
				Summary:     "Update " + k.singular,
				Tags:        []string{"admin"},
				Errors:      []int{400, 404, 500},
				Middlewares: auth,
			}, delegate(k.h.update))

			huma.Register(api, huma.Operation{
				OperationID: "admin_" + k.singular + "_delete",
				Method:      http.MethodDelete,
				Path:        nameParam,
				Summary:     "Delete " + k.singular,
				Tags:        []string{"admin"},
				Errors:      []int{404, 500},
				Middlewares: auth,
			}, delegate(k.h.del))
		}
	}

	return api
}
