// Package client is a thin Go client that speaks the relay canonical shape
// (sdk/v1). Callers build a *v1.Request once and get a *Response or a stream
// of canonical events — regardless of the target.
//
// Use Relay for the primary relay-server path (key pooling, routing, limits).
// Use For(ref, apiKey) to call any catalogued upstream host directly by model
// ref ("gpt-4o", "openai/gpt-4o", "gpt-4o@openai-direct") with zero manual
// baseURL/path/auth wiring. OpenAI, Anthropic, and Gemini constructors remain
// for explicit off-catalog targets.
//
// Configuration mirrors the OpenAI SDK: base URL, API key, auth header/scheme,
// extra default headers, request path, and HTTP client are all settable.
//
// Imports only the standard library, pkg/relay/v1, and the pure vendor
// translators — none of relay's server-side dependency graph.
package client

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Auth describes how the API key is attached to requests.
type Auth struct {
	Header string // header name, e.g. "Authorization" or "x-api-key"
	Scheme string // value prefix, e.g. "Bearer"; "" sends the raw key
}

// Client sends canonical requests to one target (relay or a vendor).
type Client struct {
	translator  v1.Translator
	baseURL     string
	path        string
	apiKey      string
	auth        Auth
	headers     map[string]string
	http        *http.Client
	transport   transport
	configErr   error                                  // deferred construction error, surfaced on the first call
	syncTimeout time.Duration                          // WR_TIMEOUT; applies to Generate only, never streams
	pathFn      func(model string, stream bool) string // per-call path (Gemini); overrides path when set
	target      Target                                 // set by For(); zero for Relay/manual constructors
}

// Option configures a Client. Options apply over the preset defaults.
type Option func(*Client)

// WithHTTPClient overrides the *http.Client. Streaming needs a client without a
// short overall timeout.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithAuth overrides the auth header/scheme (e.g. to send a raw token).
func WithAuth(a Auth) Option { return func(c *Client) { c.auth = a } }

// WithPath overrides the request path (e.g. an Azure-style deployment path).
func WithPath(p string) Option { return func(c *Client) { c.path = p } }

// WithPathFn sets a per-call path resolver, for targets that encode the model
// or the stream/sync choice in the URL path (e.g. Gemini). When set it
// overrides the static path. The model is the request's first model ref.
func WithPathFn(fn func(model string, stream bool) string) Option {
	return func(c *Client) { c.pathFn = fn }
}

// WithHeader sets one extra default header sent on every request (e.g. an
// OpenAI-Organization header, or anthropic-version override).
func WithHeader(k, v string) Option {
	return func(c *Client) {
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[k] = v
	}
}

// New builds a client for an arbitrary translator/target — the extension point
// for future adapters. Defaults to Authorization: Bearer auth.
func New(translator v1.Translator, baseURL, path, apiKey string, opts ...Option) *Client {
	c := &Client{
		translator: translator,
		baseURL:    strings.TrimRight(baseURL, "/"),
		path:       path,
		apiKey:     apiKey,
		auth:       Auth{Header: "Authorization", Scheme: "Bearer"},
		http:       http.DefaultClient,
		transport:  httpTransport{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Close releases the client's transport. For the default HTTP transport
// it is a no-op; for a WebSocket client (RelayWS) it closes the
// connection. Safe to call once when done with the client.
func (c *Client) Close() error { return c.transport.Close() }

// Relay targets a relay server's canonical endpoint (POST /v1/generate). The
// primary use: full key pooling, routing, limits, and observability.
//
// Empty baseURL/relayKey fall back to WR_BASE_URL / WR_API_KEY; WR_USAGE,
// WR_HEADERS, and WR_TIMEOUT supply further defaults. Explicit opts override
// env-derived headers. Any missing/invalid config is returned on the client
// anyway and surfaces on the first Generate/GenerateStream call — never at
// construction.
func Relay(baseURL, relayKey string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = os.Getenv(EnvBaseURL)
	}
	if relayKey == "" {
		relayKey = os.Getenv(EnvAPIKey)
	}

	// env-derived opts apply first so explicit opts override them.
	envOpts, envErr := relayEnvOptions()
	c := New(v1.IdentityTranslator{}, baseURL, "/v1/generate", relayKey, append(envOpts, opts...)...)

	if d := os.Getenv(EnvTimeout); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			envErr = errors.Join(envErr, fmt.Errorf("relay client: invalid %s %q: %w", EnvTimeout, d, err))
		} else {
			c.syncTimeout = dur
		}
	}

	var missing []string
	if c.baseURL == "" {
		missing = append(missing, EnvBaseURL)
	}
	if c.apiKey == "" {
		missing = append(missing, EnvAPIKey)
	}
	var missErr error
	if len(missing) > 0 {
		missErr = fmt.Errorf("relay client: missing config — set %s or pass explicitly", strings.Join(missing, " and "))
	}
	c.configErr = errors.Join(missErr, envErr)
	return c
}

// OpenAI targets the OpenAI Chat Completions API directly (also Ollama and any
// OpenAI-compatible host — point baseURL at it). Bypasses relay. Empty apiKey
// falls back to OPENAI_API_KEY.
func OpenAI(baseURL, apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		apiKey = os.Getenv(EnvOpenAIKey)
	}
	return newFromAdapter(adapters["openai"], baseURL, apiKey, opts...)
}

// OpenAIResponses targets the OpenAI Responses API (`/responses`) directly, the
// same wire the Codex/ChatGPT subscription backend speaks. Point baseURL at the
// host (e.g. https://chatgpt.com/backend-api/codex). Bypasses relay. Empty
// apiKey falls back to OPENAI_API_KEY; for OAuth, pass a placeholder + WithAuth
// (Auth{}) and supply the bearer via WithHTTPClient. The Responses translator
// always sends store:false, so no server-side persistence is requested.
func OpenAIResponses(baseURL, apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		apiKey = os.Getenv(EnvOpenAIKey)
	}
	return newFromAdapter(adapters["openai_responses"], baseURL, apiKey, opts...)
}

// Anthropic targets the Anthropic Messages API directly. Bypasses relay. Empty
// apiKey falls back to ANTHROPIC_API_KEY.
func Anthropic(baseURL, apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		apiKey = os.Getenv(EnvAnthropicKey)
	}
	return newFromAdapter(adapters["anthropic"], baseURL, apiKey, opts...)
}

// Gemini targets the Gemini native generateContent API directly. Bypasses
// relay. Point baseURL at the API host (e.g.
// https://generativelanguage.googleapis.com). Empty apiKey falls back to
// GEMINI_API_KEY, then GOOGLE_API_KEY. Auth is the raw key in x-goog-api-key.
func Gemini(baseURL, apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		if apiKey = os.Getenv(EnvGeminiKey); apiKey == "" {
			apiKey = os.Getenv(EnvGoogleKey)
		}
	}
	return newFromAdapter(adapters["gemini"], baseURL, apiKey, opts...)
}

func newFromAdapter(a Adapter, baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		http:      http.DefaultClient,
		transport: httpTransport{},
	}
	a.apply(c)
	for _, o := range opts {
		o(c)
	}
	return c
}
