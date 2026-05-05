package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

const relayVersion = "0.1.0"

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

// mountHuma wraps chiRouter in a humachi-backed huma API and registers all
// public operations. Returns the huma API (used in tests to inspect the spec).
//
// Routing pattern: all business-logic handlers live as huma operations on the
// top-level chi router; auth is enforced per-operation via humaAuth (not via a
// chi Group). /openapi.json, /docs, /schemas are served unauthenticated by
// huma on the same router.
//
// adminH may be nil (admin not configured); its op is skipped in that case.
func mountHuma(
	chiRouter chi.Router,
	authMW func(http.Handler) http.Handler,
	healthzH http.HandlerFunc,
	chatH http.HandlerFunc,
	modelsH http.HandlerFunc,
	adminH http.HandlerFunc,
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
	type chatInput struct {
		RawBody json.RawMessage `doc:"OpenAI-compatible chat completion request (see https://platform.openai.com/docs/api-reference/chat/create)."`
	}
	delegateBody := func(h http.HandlerFunc) func(context.Context, *chatInput) (*huma.StreamResponse, error) {
		return func(_ context.Context, _ *chatInput) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{
				Body: func(ctx huma.Context) {
					r, w := humachi.Unwrap(ctx)
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

	return api
}
