package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/control"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/keypool"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
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

// adminCRUD bundles the typed kind factories, deps, and store for mountHuma.
type adminCRUD struct {
	kinds   *adminKinds
	deps    *crud.Deps
	pgStore *catalog.PGStore // for secrets/attachment typed handlers
	kvStore kv.Store         // for best-effort Redis cleanup on secret delete
}

// mountHuma wraps chiRouter in a humachi-backed huma API for the data plane
// and registers /healthz and the /v1/* operations. Control-plane operations
// live on a separate huma API mounted by mountControlHuma.
func mountHuma(
	chiRouter chi.Router,
	authMW func(http.Handler) http.Handler,
	healthzH http.HandlerFunc,
	chatH http.HandlerFunc,
	modelsH http.HandlerFunc,
	messagesH http.HandlerFunc,
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

	// delegateMessagesBody wraps an http.HandlerFunc as a huma stream handler for Anthropic shape.
	type anthropicInput struct {
		RawBody   json.RawMessage   `doc:"Anthropic-compatible messages request."`
		Model     string            `json:"model" doc:"ID of the model to use (required)."`
		MaxTokens int               `json:"max_tokens" doc:"Maximum number of tokens to generate (required)."`
		Stream    bool              `json:"stream,omitempty" doc:"If true, events are sent as SSE."`
		Metadata  map[string]string `json:"metadata,omitempty" doc:"Optional metadata including user_id."`
	}
	delegateMessagesBody := func(h http.HandlerFunc) func(context.Context, *anthropicInput) (*huma.StreamResponse, error) {
		return func(_ context.Context, inp *anthropicInput) (*huma.StreamResponse, error) {
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

	// POST /v1/messages (Anthropic API)
	huma.Register(api, huma.Operation{
		OperationID: "create-message",
		Method:      http.MethodPost,
		Path:        "/v1/messages",
		Summary:     "Create message (Anthropic)",
		Description: "Proxies to the configured Anthropic upstream following the Anthropic Messages " +
			"API shape (https://docs.anthropic.com/en/api/messages). " +
			"Returns text/event-stream when stream=true, application/json otherwise. " +
			"Accepts x-api-key header in addition to Authorization: Bearer for SDK compatibility.",
		Tags:        []string{"anthropic"},
		Errors:      []int{400, 401, 404, 429, 500},
		Middlewares: auth,
	}, delegateMessagesBody(messagesH))

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

	return api
}

// mountControlHuma wraps controlRouter in a separate humachi-backed huma API
// for the control plane and registers every /control/* operation: login,
// logout, whoami, reload, and the full admin CRUD surface. The control API
// has its own /openapi.json and /docs distinct from the data plane.
//
// adminH may be nil (reload not configured); its op is skipped.
// crudArg may be nil; its ops are skipped.
// idStore may be nil (login disabled).
func mountControlHuma(
	controlRouter chi.Router,
	adminH http.HandlerFunc,
	crudArg *adminCRUD,
	adminTok string,
	idStore *identity.Store,
) huma.API {
	cfg := huma.DefaultConfig("Wyolet Relay — Control API", relayVersion)
	cfg.Info.Description = "Operator-facing control plane. Manages providers, pools, secrets, models, routes, " +
		"rate limits, and identity. Authentication is cookie-based: POST /control/login with " +
		"{username, password} sets relay_admin; subsequent calls send the cookie automatically. " +
		"Machine clients may also pass X-Relay-Admin-Token or Authorization: Bearer."

	api := humachi.New(controlRouter, cfg)
	adminAuth := huma.Middlewares{humaAuth(adminTokenGate(adminTok))}

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

	// GET /healthz — unauthenticated liveness probe for the control listener.
	type healthzOutput struct {
		Body struct {
			Status string `json:"status" doc:"\"ok\" while the control listener is serving."`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "control-healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Health check",
		Description: "Liveness probe for the control listener. Returns 200 while the process is up.",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*healthzOutput, error) {
		out := &healthzOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	// POST /control/login — username + password, sets cookie on success.
	type loginInput struct {
		Body struct {
			Username string `json:"username" doc:"User name as declared in the User YAML." minLength:"1"`
			Password string `json:"password" doc:"User password (plain over TLS)." minLength:"1"`
		}
	}
	type loginBodyOut struct {
		Username string   `json:"username" doc:"Authenticated user name."`
		Roles    []string `json:"roles,omitempty" doc:"Roles attached to the user."`
	}
	type loginOutput struct {
		SetCookie string `header:"Set-Cookie" doc:"Session cookie set on success."`
		Body      loginBodyOut
	}
	huma.Register(api, huma.Operation{
		OperationID: "control-login",
		Method:      http.MethodPost,
		Path:        "/control/login",
		Summary:     "Login (username + password)",
		Description: "Validates credentials against the identity store and sets a relay_admin session cookie " +
			"(HttpOnly, Secure, SameSite=Strict, 24 h). Returns 401 on bad credentials.",
		Tags:   []string{"control"},
		Errors: []int{400, 401, 503},
	}, func(_ context.Context, in *loginInput) (*loginOutput, error) {
		if idStore == nil || adminTok == "" {
			return nil, huma.NewError(http.StatusServiceUnavailable, "login not configured")
		}
		u, err := control.ValidateLogin(idStore, in.Body.Username, in.Body.Password)
		if err != nil {
			return nil, huma.NewError(http.StatusUnauthorized, "invalid credentials")
		}
		return &loginOutput{
			SetCookie: control.NewSessionCookie(adminTok).String(),
			Body:      loginBodyOut{Username: u.Spec.Username.Get(), Roles: u.Spec.Roles},
		}, nil
	})

	// POST /control/logout — clears cookie. Gated so anonymous probing returns 401.
	huma.Register(api, huma.Operation{
		OperationID:   "control-logout",
		Method:        http.MethodPost,
		Path:          "/control/logout",
		Summary:       "Logout",
		Description:   "Clears the relay_admin session cookie. Requires an active session.",
		Tags:          []string{"control"},
		Errors:        []int{401},
		Middlewares:   adminAuth,
		DefaultStatus: http.StatusNoContent,
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{
			Body: func(ctx huma.Context) {
				_, w := humachi.Unwrap(ctx)
				http.SetCookie(w, control.NewClearCookie())
				w.WriteHeader(http.StatusNoContent)
			},
		}, nil
	})

	// GET /control/whoami — gated; reports authenticated status.
	type whoamiOutput struct {
		Body struct {
			Authenticated bool `json:"authenticated" doc:"Always true when this gated endpoint responds."`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "control-whoami",
		Method:      http.MethodGet,
		Path:        "/control/whoami",
		Summary:     "Whoami",
		Description: "Returns {authenticated: true} if the session is valid.",
		Tags:        []string{"control"},
		Errors:      []int{401},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*whoamiOutput, error) {
		out := &whoamiOutput{}
		out.Body.Authenticated = true
		return out, nil
	})

	// POST /control/reload — admin reload handler.
	if adminH != nil {
		huma.Register(api, huma.Operation{
			OperationID:   "control-reload",
			Method:        http.MethodPost,
			Path:          "/control/reload",
			Summary:       "Reload catalog",
			Description:   "Triggers a live config reload from the Postgres catalog.",
			Tags:          []string{"control"},
			Errors:        []int{401, 429, 500},
			Middlewares:   adminAuth,
			DefaultStatus: http.StatusOK,
		}, delegate(adminH))
	}

	// CRUD + secrets + attachments + misc.
	if crudArg != nil {
		crud.RegisterOps(api, "/control/providers", "provider", "providers",
			crudArg.kinds.provider, *crudArg.deps, adminAuth)
		crud.RegisterOps(api, "/control/pools", "pool", "pools",
			crudArg.kinds.pool, *crudArg.deps, adminAuth)
		crud.RegisterOps(api, "/control/models", "model", "models",
			crudArg.kinds.model, *crudArg.deps, adminAuth)
		crud.RegisterOps(api, "/control/routes", "route", "routes",
			crudArg.kinds.route, *crudArg.deps, adminAuth)
		crud.RegisterOps(api, "/control/ratelimits", "ratelimit", "ratelimits",
			crudArg.kinds.rateLimit, *crudArg.deps, adminAuth)

		if crudArg.pgStore != nil {
			registerTypedSecretOps(api, crudArg.pgStore, crudArg.deps, crudArg.kvStore, adminAuth)
			registerTypedAttachmentOps(api, crudArg.pgStore, adminAuth)
		}
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

// secretPatch builds a catalog.Patch for pre-commit snapshot validation of a
// secret write. The patch must mirror the value mode (env-ref or stored) the
// real upsert will apply, otherwise the validator rejects the synthetic Secret
// for "exactly one of valueFrom.env or value required".
func secretPatch(inp SecretWriteBody) catalog.Patch {
	provider := inp.Provider
	if provider == "" {
		provider = "default"
	}
	spec := catalog.SecretSpec{Provider: provider}
	switch inp.ValueFrom.Kind {
	case "env":
		spec.ValueFrom = &catalog.SecretValueFrom{Env: inp.ValueFrom.Env}
	case "stored":
		spec.Value = inp.ValueFrom.Value
	}
	return catalog.Patch{
		UpsertSecret: &catalog.Secret{
			Metadata: catalog.Metadata{Name: inp.Name},
			Spec:     spec,
		},
	}
}

func registerTypedSecretOps(api huma.API, store *catalog.PGStore, deps *crud.Deps, kvStore kv.Store, adminAuth huma.Middlewares) {
	// List
	huma.Register(api, huma.Operation{
		OperationID: "admin_secret_list",
		Method:      http.MethodGet,
		Path:        "/control/secrets",
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
		Path:        "/control/secrets/{name}",
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
		Path:          "/control/secrets",
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
		if verr := deps.Patcher.ValidateWithPatch(secretPatch(inp)); verr != nil {
			return nil, huma.NewError(http.StatusBadRequest, verr.Error())
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return applySecretWriteToTx(ctx, store, inp.Name, inp)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			slog.WarnContext(ctx, "admin: reload failed after secret create; snapshot may be stale",
				"name", inp.Name, "err", err)
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
		Path:        "/control/secrets/{name}",
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
		if verr := deps.Patcher.ValidateWithPatch(secretPatch(inp)); verr != nil {
			return nil, huma.NewError(http.StatusBadRequest, verr.Error())
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return applySecretWriteToTx(ctx, store, in.Name, inp)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			slog.WarnContext(ctx, "admin: reload failed after secret update; snapshot may be stale",
				"name", in.Name, "err", err)
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
		Path:          "/control/secrets/{name}",
		Summary:       "Delete secret",
		Tags:          []string{"admin"},
		Errors:        []int{400, 404, 500},
		Middlewares:   adminAuth,
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *secretNamePathInput) (*struct{}, error) {
		sec, ok := store.SecretByName(in.Name)
		if !ok {
			return nil, huma.NewError(http.StatusNotFound, fmt.Sprintf("Secret %q not found", in.Name))
		}
		keyHash := sec.KeyHash
		if verr := deps.Patcher.ValidateWithPatch(catalog.Patch{DeleteSecret: in.Name}); verr != nil {
			return nil, huma.NewError(http.StatusBadRequest, verr.Error())
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return store.DeleteSecret(ctx, in.Name)
		}); err != nil {
			return nil, huma.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			slog.WarnContext(ctx, "admin: reload failed after secret delete; snapshot may be stale",
				"name", in.Name, "err", err)
		}
		// Best-effort: delete the orphaned circuit-breaker key from Redis (R-8).
		// The catalog write already succeeded; a Redis error is non-fatal.
		if kvStore != nil && keyHash != "" {
			if err := keypool.ClearCircuit(ctx, kvStore, keyHash); err != nil {
				slog.Warn("secret delete: could not clear circuit-breaker key from Redis",
					"secret", in.Name, "keyHash", keyHash, "err", err)
			}
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
		Path:        "/control/attachments",
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
					ID:            kind + ":" + name + ":" + a.Ref,
					ParentKind:    kind,
					ParentName:    name,
					RatelimitName: a.Ref,
					Meter:         "", // meter is now owned by the RateLimit rules, not the attachment
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
		Path:        "/control/version",
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
		Path:        "/control/master-key/generate",
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
