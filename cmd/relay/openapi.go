package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/crypto"
)

const relayVersion = "0.1.0"

func init() {
	// Override huma's default error model to produce OpenAI-compatible envelopes.
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
type openAIError struct {
	Err        openAIErrorInner `json:"error"`
	httpStatus int
}

func (e *openAIError) GetStatus() int              { return e.httpStatus }
func (e *openAIError) Error() string               { return e.Err.Message }
func (e *openAIError) ContentType(_ string) string { return "application/json" }

// humaAuth converts a net/http middleware into a huma per-operation middleware.
func humaAuth(authMW func(http.Handler) http.Handler) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humachi.Unwrap(ctx)
		authMW(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			next(humachi.NewContext(ctx.Operation(), r2, w2))
		})).ServeHTTP(w, r)
	}
}

// adminHandlers holds the five http.HandlerFuncs produced by the crud factory for one kind.
// Used by mountAdminRoutes (chi) only; mountHuma uses RegisterOps directly when kinds are set.
type adminHandlers struct {
	list   http.HandlerFunc
	get    http.HandlerFunc
	create http.HandlerFunc
	update http.HandlerFunc
	del    http.HandlerFunc
}

// adminCRUD bundles all five kinds' handler sets, typed kind factories, and deps.
type adminCRUD struct {
	// chi http.HandlerFuncs — used by mountAdminRoutes.
	provider  adminHandlers
	pool      adminHandlers
	model     adminHandlers
	route     adminHandlers
	rateLimit adminHandlers

	secretList     http.HandlerFunc
	secretGet      http.HandlerFunc
	secretCreate   http.HandlerFunc
	secretUpdate   http.HandlerFunc
	secretDelete   http.HandlerFunc
	attachmentList http.HandlerFunc
	version        http.HandlerFunc
	masterKeyGenerate http.HandlerFunc

	// Typed kind factories — used by mountHuma for full OpenAPI schema generation.
	// Nil when admin is not configured or when built from stubs (tests).
	kinds *adminKinds
	deps  *crud.Deps
	pgStore *catalog.PGStore // for secrets/attachment typed handlers
}

// mountHuma wraps chiRouter in a humachi-backed huma API and registers all operations.
// Returns the huma API (used in tests to inspect the spec).
//
// adminH may be nil (admin not configured); its op is skipped.
// crudArg may be nil; its ops are skipped.
func mountHuma(
	chiRouter chi.Router,
	authMW func(http.Handler) http.Handler,
	healthzH http.HandlerFunc,
	chatH http.HandlerFunc,
	modelsH http.HandlerFunc,
	adminH http.HandlerFunc,
	crudArg *adminCRUD,
	adminTok string,
) huma.API {
	cfg := huma.DefaultConfig("Wyolet Relay", relayVersion)
	cfg.Info.Description = "High-throughput LLM router. " +
		"Chat-completions and models endpoints follow the OpenAI API shape " +
		"(https://platform.openai.com/docs/api-reference). " +
		"Only `model`, `stream`, and `user` are inspected by Relay; " +
		"all other fields are forwarded verbatim to the upstream provider."

	api := humachi.New(chiRouter, cfg)
	auth := huma.Middlewares{humaAuth(authMW)}
	adminAuth := huma.Middlewares{humaAuth(adminTokenGate(adminTok))}

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
		RawBody  json.RawMessage   `doc:"OpenAI-compatible chat completion request."`
		Model    string            `json:"model" doc:"ID of the model to use (required)."`
		Stream   bool              `json:"stream,omitempty" doc:"If true, partial message deltas are sent as SSE."`
		User     string            `json:"user,omitempty" doc:"Caller identifier forwarded to the upstream provider."`
		Metadata map[string]string `json:"metadata,omitempty" doc:"Up to 16 key/value pairs for caller attribution."`
	}
	delegateBody := func(h http.HandlerFunc) func(context.Context, *chatInput) (*huma.StreamResponse, error) {
		return func(_ context.Context, inp *chatInput) (*huma.StreamResponse, error) {
			raw := inp.RawBody
			return &huma.StreamResponse{
				Body: func(ctx huma.Context) {
					r, w := humachi.Unwrap(ctx)
					r.Body = io.NopCloser(bytes.NewReader(raw))
					r.ContentLength = int64(len(raw))
					h.ServeHTTP(w, r)
				},
			}, nil
		}
	}

	// GET /healthz
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

	// POST /v1/chat/completions
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

	// Patch the generated OpenAPI spec for /v1/chat/completions.
	if op, ok := api.OpenAPI().Paths["/v1/chat/completions"]; ok {
		if op.Post != nil && op.Post.RequestBody != nil {
			op.Post.RequestBody.Content = map[string]*huma.MediaType{
				"application/json": {
					Schema: &huma.Schema{
						Type: "object",
						Properties: map[string]*huma.Schema{
							"model":  {Type: "string", Description: "ID of the model to use (required)."},
							"stream": {Type: "boolean", Description: "If true, partial message deltas are sent as SSE."},
							"user":   {Type: "string", Description: "Caller identifier forwarded to the upstream provider."},
							"metadata": {
								Type:                 "object",
								Description:          "Up to 16 key/value pairs for caller attribution.",
								AdditionalProperties: &huma.Schema{Type: "string"},
							},
						},
						Required: []string{"model"},
					},
				},
			}
		}
	}

	// GET /v1/models
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

	// POST /admin/reload
	if adminH != nil {
		huma.Register(api, huma.Operation{
			OperationID:   "admin-reload",
			Method:        http.MethodPost,
			Path:          "/admin/reload",
			Summary:       "Reload catalog",
			Description:   "Triggers a live config reload from the Postgres catalog. Requires admin bearer token.",
			Tags:          []string{"admin"},
			Errors:        []int{401, 429, 500},
			Middlewares:   adminAuth,
			DefaultStatus: http.StatusOK,
		}, delegate(adminH))
	}

	// POST /admin/login
	type loginBody struct {
		Token string `json:"token" doc:"Admin token." minLength:"1"`
	}
	type loginInput struct {
		Body loginBody
	}
	type loginOutput struct {
		SetCookie string `header:"Set-Cookie" doc:"Session cookie set on success."`
		Body      struct{}
	}

	tok := []byte(adminTok)
	huma.Register(api, huma.Operation{
		OperationID: "admin_login",
		Method:      http.MethodPost,
		Path:        "/admin/login",
		Summary:     "Admin login (cookie auth)",
		Description: "Validates the admin token and sets a relay_admin session cookie (HttpOnly, Secure, SameSite=Strict, 24 h). Returns 401 on wrong token.",
		Tags:        []string{"admin"},
		Errors:      []int{400, 401},
	}, func(_ context.Context, in *loginInput) (*loginOutput, error) {
		if subtle.ConstantTimeCompare([]byte(in.Body.Token), tok) != 1 {
			return nil, huma.NewError(http.StatusUnauthorized, "invalid admin token")
		}
		cookie := &http.Cookie{
			Name:     adminLoginCookieName,
			Value:    in.Body.Token,
			Path:     "/",
			MaxAge:   86400,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		}
		return &loginOutput{SetCookie: cookie.String()}, nil
	})

	// POST /admin/logout — uses StreamResponse to set the clear-cookie header.
	huma.Register(api, huma.Operation{
		OperationID:   "admin_logout",
		Method:        http.MethodPost,
		Path:          "/admin/logout",
		Summary:       "Admin logout",
		Description:   "Clears the relay_admin session cookie. Requires an active session.",
		Tags:          []string{"admin"},
		Errors:        []int{401},
		Middlewares:   adminAuth,
		DefaultStatus: http.StatusNoContent,
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{
			Body: func(ctx huma.Context) {
				_, w := humachi.Unwrap(ctx)
				http.SetCookie(w, &http.Cookie{
					Name:     adminLoginCookieName,
					Value:    "",
					Path:     "/",
					MaxAge:   0,
					HttpOnly: true,
					Secure:   true,
					SameSite: http.SameSiteStrictMode,
				})
				w.WriteHeader(http.StatusNoContent)
			},
		}, nil
	})

	// GET /admin/whoami
	type whoamiOutput struct {
		Body struct {
			Authenticated bool `json:"authenticated" doc:"Always true when this gated endpoint responds."`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "admin_whoami",
		Method:      http.MethodGet,
		Path:        "/admin/whoami",
		Summary:     "Admin whoami",
		Description: "Returns {authenticated: true} if the admin session is valid.",
		Tags:        []string{"admin"},
		Errors:      []int{401},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*whoamiOutput, error) {
		out := &whoamiOutput{}
		out.Body.Authenticated = true
		return out, nil
	})

	// Admin CRUD
	if crudArg != nil {
		if crudArg.kinds != nil && crudArg.deps != nil {
			// Full typed registration with schema generation.
			crud.RegisterOps(api, "/admin/providers", "provider", "providers",
				crudArg.kinds.provider, *crudArg.deps, adminAuth)
			crud.RegisterOps(api, "/admin/pools", "pool", "pools",
				crudArg.kinds.pool, *crudArg.deps, adminAuth)
			crud.RegisterOps(api, "/admin/models", "model", "models",
				crudArg.kinds.model, *crudArg.deps, adminAuth)
			crud.RegisterOps(api, "/admin/routes", "route", "routes",
				crudArg.kinds.route, *crudArg.deps, adminAuth)
			crud.RegisterOps(api, "/admin/ratelimits", "ratelimit", "ratelimits",
				crudArg.kinds.rateLimit, *crudArg.deps, adminAuth)
		} else {
			// Fallback: delegate-based registration (no body schemas).
			// Used in openapi_test.go stubs.
			type kindSpec struct {
				singular string
				plural   string
				h        adminHandlers
			}
			kinds := []kindSpec{
				{"provider", "providers", crudArg.provider},
				{"pool", "pools", crudArg.pool},
				{"model", "models", crudArg.model},
				{"route", "routes", crudArg.route},
				{"ratelimit", "ratelimits", crudArg.rateLimit},
			}
			for _, k := range kinds {
				k := k
				base := "/admin/" + k.plural
				nameParam := base + "/{name}"
				huma.Register(api, huma.Operation{
					OperationID: "admin_" + k.singular + "_list",
					Method: http.MethodGet, Path: base,
					Summary: "List " + k.plural, Tags: []string{"admin"},
					Errors: []int{500}, Middlewares: adminAuth,
				}, delegate(k.h.list))
				huma.Register(api, huma.Operation{
					OperationID: "admin_" + k.singular + "_get",
					Method: http.MethodGet, Path: nameParam,
					Summary: "Get " + k.singular, Tags: []string{"admin"},
					Errors: []int{404, 500}, Middlewares: adminAuth,
				}, delegate(k.h.get))
				huma.Register(api, huma.Operation{
					OperationID: "admin_" + k.singular + "_create",
					Method: http.MethodPost, Path: base,
					Summary: "Create " + k.singular, Tags: []string{"admin"},
					Errors: []int{400, 500}, Middlewares: adminAuth,
				}, delegate(k.h.create))
				huma.Register(api, huma.Operation{
					OperationID: "admin_" + k.singular + "_update",
					Method: http.MethodPut, Path: nameParam,
					Summary: "Update " + k.singular, Tags: []string{"admin"},
					Errors: []int{400, 404, 500}, Middlewares: adminAuth,
				}, delegate(k.h.update))
				huma.Register(api, huma.Operation{
					OperationID: "admin_" + k.singular + "_delete",
					Method: http.MethodDelete, Path: nameParam,
					Summary: "Delete " + k.singular, Tags: []string{"admin"},
					Errors: []int{404, 500}, Middlewares: adminAuth,
				}, delegate(k.h.del))
			}
		}

		// --- Secret endpoints ---
		if crudArg.pgStore != nil && crudArg.deps != nil {
			registerTypedSecretOps(api, crudArg.pgStore, crudArg.deps, adminAuth)
		} else {
			huma.Register(api, huma.Operation{
				OperationID: "admin_secret_list", Method: http.MethodGet, Path: "/admin/secrets",
				Summary: "List secrets", Tags: []string{"admin"}, Errors: []int{500}, Middlewares: adminAuth,
			}, delegate(crudArg.secretList))
			huma.Register(api, huma.Operation{
				OperationID: "admin_secret_get", Method: http.MethodGet, Path: "/admin/secrets/{name}",
				Summary: "Get secret", Tags: []string{"admin"}, Errors: []int{404, 500}, Middlewares: adminAuth,
			}, delegate(crudArg.secretGet))
			huma.Register(api, huma.Operation{
				OperationID: "admin_secret_create", Method: http.MethodPost, Path: "/admin/secrets",
				Summary: "Create secret", Tags: []string{"admin"}, Errors: []int{400, 500}, Middlewares: adminAuth,
			}, delegate(crudArg.secretCreate))
			huma.Register(api, huma.Operation{
				OperationID: "admin_secret_update", Method: http.MethodPut, Path: "/admin/secrets/{name}",
				Summary: "Update secret", Tags: []string{"admin"}, Errors: []int{400, 404, 500}, Middlewares: adminAuth,
			}, delegate(crudArg.secretUpdate))
			huma.Register(api, huma.Operation{
				OperationID: "admin_secret_delete", Method: http.MethodDelete, Path: "/admin/secrets/{name}",
				Summary: "Delete secret", Tags: []string{"admin"}, Errors: []int{404, 500}, Middlewares: adminAuth,
			}, delegate(crudArg.secretDelete))
		}

		// --- Attachment endpoint ---
		if crudArg.pgStore != nil {
			registerTypedAttachmentOps(api, crudArg.pgStore, adminAuth)
		} else {
			huma.Register(api, huma.Operation{
				OperationID: "admin_attachment_list", Method: http.MethodGet, Path: "/admin/attachments",
				Summary: "List attachments (derived, read-only)",
				Description: "Returns all rate-limit attachments derived from inline rateLimits on Pool/Secret/Model specs.",
				Tags: []string{"admin"}, Errors: []int{400, 500}, Middlewares: adminAuth,
			}, delegate(crudArg.attachmentList))
		}

		// --- Misc ---
		registerTypedMiscOps(api, adminAuth)
	}

	return api
}

// ============================================================================
// Typed Secret handlers
// ============================================================================

// SecretValueFromResponse is the read-only response shape for a secret's valueFrom.
type SecretValueFromResponse struct {
	Kind        string `json:"kind" doc:"'env' or 'stored'."`
	Env         string `json:"env,omitempty" doc:"Environment variable name (env-mode only)."`
	ValueMasked string `json:"value_masked,omitempty" doc:"Last 4 chars of the secret value with prefix (stored-mode only). Cleartext is never returned."`
}

// SecretResponse is the read-only response shape for a secret resource.
type SecretResponse struct {
	Name      string                  `json:"name" doc:"Secret name."`
	ValueFrom SecretValueFromResponse `json:"valueFrom" doc:"Secret source configuration."`
}

// SecretValueFromInput is the write-side shape for a secret's valueFrom.
type SecretValueFromInput struct {
	Kind  string `json:"kind" doc:"'env' (reference to env var) or 'stored' (encrypted in Postgres)." enum:"env,stored"`
	Env   string `json:"env,omitempty" doc:"Env var name (required when kind='env')."`
	Value string `json:"value,omitempty" doc:"Cleartext secret value (required when kind='stored'). Encrypted with RELAY_MASTER_KEY before storage. Never returned in responses."`
}

// SecretWriteBody is the request body for create/update secret.
type SecretWriteBody struct {
	Name      string               `json:"name" doc:"Secret name (unique identifier)." minLength:"1"`
	Provider  string               `json:"provider,omitempty" doc:"Provider this secret belongs to. Defaults to 'default'."`
	ValueFrom SecretValueFromInput `json:"valueFrom" doc:"Secret source configuration."`
}

type secretListOutput struct {
	Body struct {
		Items []SecretResponse `json:"items" doc:"All secrets (cleartext never included)."`
	}
}

type secretItemOutput struct {
	Body SecretResponse
}

type secretNamePathInput struct {
	Name string `path:"name" doc:"Secret name."`
}

type secretCreateInput struct {
	Body SecretWriteBody
}

type secretUpdateInput struct {
	Name string          `path:"name" doc:"Secret name."`
	Body SecretWriteBody
}

func storeToSecretResp(sec *catalog.Secret) SecretResponse {
	if sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "" {
		return SecretResponse{
			Name:      sec.Metadata.Name,
			ValueFrom: SecretValueFromResponse{Kind: "env", Env: sec.Spec.ValueFrom.Env},
		}
	}
	return SecretResponse{
		Name:      sec.Metadata.Name,
		ValueFrom: SecretValueFromResponse{Kind: "stored", ValueMasked: maskValue(sec.Resolved)},
	}
}

func validateSecretWriteBody(inp SecretWriteBody) error {
	if inp.Name == "" {
		return errors.New("name required")
	}
	if inp.ValueFrom.Kind != "env" && inp.ValueFrom.Kind != "stored" {
		return fmt.Errorf("valueFrom.kind must be \"env\" or \"stored\", got %q", inp.ValueFrom.Kind)
	}
	if inp.ValueFrom.Kind == "env" && inp.ValueFrom.Env == "" {
		return errors.New("valueFrom.env required for env-mode")
	}
	if inp.ValueFrom.Kind == "stored" && inp.ValueFrom.Value == "" {
		return errors.New("valueFrom.value required for stored-mode")
	}
	return nil
}

func applySecretWriteToTx(ctx context.Context, store *catalog.PGStore, name string, inp SecretWriteBody) error {
	provider := inp.Provider
	if provider == "" {
		provider = "default"
	}
	meta := catalog.Metadata{Name: name}
	switch inp.ValueFrom.Kind {
	case "env":
		return store.UpsertSecretEnv(ctx, name, inp.ValueFrom.Env, provider, meta)
	case "stored":
		return store.UpsertSecretStored(ctx, name, inp.ValueFrom.Value, provider, meta)
	default:
		return fmt.Errorf("unknown kind %q", inp.ValueFrom.Kind)
	}
}

func registerTypedSecretOps(api huma.API, store *catalog.PGStore, deps *crud.Deps, adminAuth huma.Middlewares) {
	// List
	huma.Register(api, huma.Operation{
		OperationID: "admin_secret_list",
		Method:      http.MethodGet,
		Path:        "/admin/secrets",
		Summary:     "List secrets",
		Tags:        []string{"admin"},
		Errors:      []int{500},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*secretListOutput, error) {
		secrets := store.Secrets()
		out := &secretListOutput{}
		out.Body.Items = make([]SecretResponse, 0, len(secrets))
		for _, s := range secrets {
			out.Body.Items = append(out.Body.Items, storeToSecretResp(s))
		}
		return out, nil
	})

	// Get
	huma.Register(api, huma.Operation{
		OperationID: "admin_secret_get",
		Method:      http.MethodGet,
		Path:        "/admin/secrets/{name}",
		Summary:     "Get secret",
		Tags:        []string{"admin"},
		Errors:      []int{404, 500},
		Middlewares: adminAuth,
	}, func(_ context.Context, in *secretNamePathInput) (*secretItemOutput, error) {
		sec, ok := store.SecretByName(in.Name)
		if !ok {
			return nil, huma.NewError(http.StatusNotFound, fmt.Sprintf("Secret %q not found", in.Name))
		}
		return &secretItemOutput{Body: storeToSecretResp(sec)}, nil
	})

	// Create
	huma.Register(api, huma.Operation{
		OperationID:   "admin_secret_create",
		Method:        http.MethodPost,
		Path:          "/admin/secrets",
		Summary:       "Create secret",
		Description:   "Creates a new secret. For stored-mode, RELAY_MASTER_KEY must be set; the value is AES-GCM-256 encrypted before storage.",
		Tags:          []string{"admin"},
		Errors:        []int{400, 500},
		Middlewares:   adminAuth,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *secretCreateInput) (*secretItemOutput, error) {
		inp := in.Body
		if err := validateSecretWriteBody(inp); err != nil {
			return nil, huma.NewError(http.StatusBadRequest, err.Error())
		}
		if inp.ValueFrom.Kind == "stored" && !store.HasMasterKey() {
			return nil, huma.NewError(http.StatusBadRequest, "stored-mode secret requires RELAY_MASTER_KEY to be set")
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return applySecretWriteToTx(ctx, store, inp.Name, inp)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, "mutation committed but reload failed: "+err.Error())
		}
		sec, ok := store.SecretByName(inp.Name)
		if !ok {
			return nil, huma.NewError(http.StatusInternalServerError, "created but could not read back")
		}
		return &secretItemOutput{Body: storeToSecretResp(sec)}, nil
	})

	// Update
	huma.Register(api, huma.Operation{
		OperationID: "admin_secret_update",
		Method:      http.MethodPut,
		Path:        "/admin/secrets/{name}",
		Summary:     "Update secret",
		Tags:        []string{"admin"},
		Errors:      []int{400, 404, 500},
		Middlewares: adminAuth,
	}, func(ctx context.Context, in *secretUpdateInput) (*secretItemOutput, error) {
		if _, ok := store.SecretByName(in.Name); !ok {
			return nil, huma.NewError(http.StatusNotFound, fmt.Sprintf("Secret %q not found", in.Name))
		}
		inp := in.Body
		inp.Name = in.Name // path param wins
		if err := validateSecretWriteBody(inp); err != nil {
			return nil, huma.NewError(http.StatusBadRequest, err.Error())
		}
		if inp.ValueFrom.Kind == "stored" && !store.HasMasterKey() {
			return nil, huma.NewError(http.StatusBadRequest, "stored-mode secret requires RELAY_MASTER_KEY to be set")
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return applySecretWriteToTx(ctx, store, in.Name, inp)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, "mutation committed but reload failed: "+err.Error())
		}
		sec, ok := store.SecretByName(in.Name)
		if !ok {
			return nil, huma.NewError(http.StatusInternalServerError, "updated but could not read back")
		}
		return &secretItemOutput{Body: storeToSecretResp(sec)}, nil
	})

	// Delete
	huma.Register(api, huma.Operation{
		OperationID:   "admin_secret_delete",
		Method:        http.MethodDelete,
		Path:          "/admin/secrets/{name}",
		Summary:       "Delete secret",
		Tags:          []string{"admin"},
		Errors:        []int{400, 404, 500},
		Middlewares:   adminAuth,
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *secretNamePathInput) (*struct{}, error) {
		if _, ok := store.SecretByName(in.Name); !ok {
			return nil, huma.NewError(http.StatusNotFound, fmt.Sprintf("Secret %q not found", in.Name))
		}
		if verr := deps.Patcher.ValidateWithPatch(catalog.Patch{DeleteSecret: in.Name}); verr != nil {
			return nil, huma.NewError(http.StatusBadRequest, verr.Error())
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return store.DeleteSecret(ctx, in.Name)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, "mutation committed but reload failed: "+err.Error())
		}
		return nil, nil
	})
}

// ============================================================================
// Typed Attachment handlers
// ============================================================================

// AttachmentResponse is the shape for a single attachment record.
type AttachmentResponse struct {
	ID            string `json:"id" doc:"Composite key: parentKind:parentName:ratelimitName:meter."`
	ParentKind    string `json:"parentKind" doc:"Resource kind that owns the rate-limit (Pool, Secret, or Model)."`
	ParentName    string `json:"parentName" doc:"Name of the parent resource."`
	RatelimitName string `json:"ratelimitName" doc:"Name of the referenced RateLimit resource."`
	Meter         string `json:"meter" doc:"Meter type: requests, tokens, or concurrency."`
}

type attachmentListOutput struct {
	Body struct {
		Items []AttachmentResponse `json:"items" doc:"All attachment records derived from inline rateLimits specs."`
	}
}

type attachmentQueryInput struct {
	ParentKind string `query:"parent_kind" doc:"Filter by parent kind (Pool, Secret, or Model). Must be combined with parent_name."`
	ParentName string `query:"parent_name" doc:"Filter by parent resource name. Must be combined with parent_kind."`
}

func registerTypedAttachmentOps(api huma.API, store *catalog.PGStore, adminAuth huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "admin_attachment_list",
		Method:      http.MethodGet,
		Path:        "/admin/attachments",
		Summary:     "List attachments (derived, read-only)",
		Description: "Returns all rate-limit attachments derived from inline rateLimits on Pool/Secret/Model specs. " +
			"Optional query params parent_kind + parent_name (both required together) filter to one parent. " +
			"To create or remove attachments, PUT the parent resource with an updated rateLimits array.",
		Tags:        []string{"admin"},
		Errors:      []int{400, 500},
		Middlewares: adminAuth,
	}, func(_ context.Context, in *attachmentQueryInput) (*attachmentListOutput, error) {
		parentKind := in.ParentKind
		parentName := in.ParentName
		if (parentKind == "") != (parentName == "") {
			return nil, huma.NewError(http.StatusBadRequest,
				"parent_kind and parent_name must be provided together (or both omitted to list all)")
		}
		if parentKind != "" {
			wk := catalog.Kind(parentKind)
			if wk != catalog.KindPool && wk != catalog.KindSecret && wk != catalog.KindModel {
				return nil, huma.NewError(http.StatusBadRequest,
					fmt.Sprintf("parent_kind %q not supported (must be Pool, Secret, or Model)", parentKind))
			}
		}

		var items []AttachmentResponse
		emit := func(kind, name string, rls []catalog.RateLimitAttachment) {
			for _, a := range rls {
				items = append(items, AttachmentResponse{
					ID:            kind + ":" + name + ":" + a.Ref + ":" + string(a.Meter),
					ParentKind:    kind,
					ParentName:    name,
					RatelimitName: a.Ref,
					Meter:         string(a.Meter),
				})
			}
		}
		wantKind := catalog.Kind(parentKind)
		if parentKind == "" || wantKind == catalog.KindPool {
			for _, p := range store.Pools() {
				if parentName != "" && p.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindPool), p.Metadata.Name, p.Spec.RateLimits)
			}
		}
		if parentKind == "" || wantKind == catalog.KindSecret {
			for _, s := range store.Secrets() {
				if parentName != "" && s.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindSecret), s.Metadata.Name, s.Spec.RateLimits)
			}
		}
		if parentKind == "" || wantKind == catalog.KindModel {
			for _, m := range store.Models() {
				if parentName != "" && m.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindModel), m.Metadata.Name, m.Spec.RateLimits)
			}
		}
		out := &attachmentListOutput{}
		if items == nil {
			items = []AttachmentResponse{}
		}
		out.Body.Items = items
		return out, nil
	})
}

// ============================================================================
// Typed Misc handlers
// ============================================================================

// VersionResponse is the response body for GET /admin/version.
type VersionResponse struct {
	Version string `json:"version" doc:"Relay release version string (semver)."`
}

// MasterKeyResponse is the response body for GET /admin/master-key/generate.
type MasterKeyResponse struct {
	Key string `json:"key" doc:"Base64-encoded 32-byte master key. Store this immediately — it cannot be recovered."`
}

type versionOutput struct {
	Body VersionResponse
}

type masterKeyOutput struct {
	Body MasterKeyResponse
}

func registerTypedMiscOps(api huma.API, adminAuth huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "admin_version",
		Method:      http.MethodGet,
		Path:        "/admin/version",
		Summary:     "Get relay version",
		Tags:        []string{"admin"},
		Errors:      []int{401},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*versionOutput, error) {
		return &versionOutput{Body: VersionResponse{Version: relayVersion}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin_master_key_generate",
		Method:      http.MethodGet,
		Path:        "/admin/master-key/generate",
		Summary:     "Generate a fresh master key",
		Description: "Returns a freshly generated 32-byte master key, base64-encoded. " +
			"Relay does not persist it — operator must store it in their secret store before navigating away.",
		Tags:        []string{"admin"},
		Errors:      []int{401, 500},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*masterKeyOutput, error) {
		key, err := crypto.GenerateMasterKey()
		if err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		return &masterKeyOutput{Body: MasterKeyResponse{Key: key}}, nil
	})
}
