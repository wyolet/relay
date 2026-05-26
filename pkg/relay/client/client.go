// Package client is a thin, dependency-free Go client that speaks the relay
// canonical shape (pkg/relay/v1). Callers build a *v1.Request once and get a
// *v1.Response or a stream of canonical events — regardless of the target.
//
// The client does not care whether it talks to a relay server or directly to a
// vendor: a target is just a v1.Translator + base URL + path + auth. Relay is
// the primary target (POST /v1/generate, identity translator), but the same
// client bridges straight to OpenAI, Anthropic, Ollama, or any future adapter
// by swapping the translator — relay's own dispatchCanonical chain, run
// client-side. Direct-to-vendor bypasses relay's key pooling, rate limiting,
// breakers, and observability; it's a local-dev / offline / fallback path.
//
// Configuration mirrors the OpenAI SDK: base URL, API key, auth header/scheme,
// extra default headers, request path, and HTTP client are all settable.
//
// Imports only the standard library, pkg/relay/v1, and the pure vendor
// translators — none of relay's server-side dependency graph.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/adapters/anthropic"
	"github.com/wyolet/relay/pkg/adapters/openai"
	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// Auth describes how the API key is attached to requests.
type Auth struct {
	Header string // header name, e.g. "Authorization" or "x-api-key"
	Scheme string // value prefix, e.g. "Bearer"; "" sends the raw key
}

// Client sends canonical requests to one target (relay or a vendor).
type Client struct {
	translator v1.Translator
	baseURL    string
	path       string
	apiKey     string
	auth       Auth
	headers    map[string]string
	http       *http.Client
	transport  transport
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
func Relay(baseURL, relayKey string, opts ...Option) *Client {
	return New(v1.IdentityTranslator{}, baseURL, "/v1/generate", relayKey, opts...)
}

// OpenAI targets the OpenAI Chat Completions API directly (also Ollama and any
// OpenAI-compatible host — point baseURL at it). Bypasses relay.
func OpenAI(baseURL, apiKey string, opts ...Option) *Client {
	return New(openai.CCTranslator{}, baseURL, "/v1/chat/completions", apiKey, opts...)
}

// Anthropic targets the Anthropic Messages API directly. Bypasses relay.
func Anthropic(baseURL, apiKey string, opts ...Option) *Client {
	withAnthropicDefaults := append([]Option{
		WithAuth(Auth{Header: "x-api-key"}),
		WithHeader("anthropic-version", "2023-06-01"),
	}, opts...)
	return New(anthropic.AnthropicTranslator{}, baseURL, "/v1/messages", apiKey, withAnthropicDefaults...)
}

// Generate runs a non-streaming generation. OutputMode is forced to sync; the
// caller's request is not mutated.
func (c *Client) Generate(ctx context.Context, req *v1.Request) (*v1.Response, error) {
	resp, err := c.roundTrip(ctx, req, v1.OutputModeSync)
	if err != nil {
		return nil, err
	}
	defer resp.body.Close()

	body, err := io.ReadAll(resp.body)
	if err != nil {
		return nil, fmt.Errorf("relay client: read body: %w", err)
	}
	if resp.status/100 != 2 {
		return nil, parseAPIError(resp.status, body)
	}
	return c.translator.ParseResponse(body)
}

// GenerateStream runs a streaming generation. The returned *Stream yields
// canonical events until io.EOF; the caller must Close it. OutputMode is forced
// to stream.
func (c *Client) GenerateStream(ctx context.Context, req *v1.Request) (*Stream, error) {
	start := time.Now() // anchor for reasoning-span offsets, like relay's request-accept
	resp, err := c.roundTrip(ctx, req, v1.OutputModeStream)
	if err != nil {
		return nil, err
	}
	if resp.status/100 != 2 {
		body, _ := io.ReadAll(resp.body)
		_ = resp.body.Close()
		return nil, parseAPIError(resp.status, body)
	}
	sc := bufio.NewScanner(resp.body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(splitSSEFrames)
	return &Stream{body: resp.body, sc: sc, toCanon: c.translator.NewToCanonicalStream(), start: start}, nil
}

// roundTrip serializes the request (translator-owned) and hands the bytes
// to the transport. The caller's request is not mutated.
func (c *Client) roundTrip(ctx context.Context, req *v1.Request, mode string) (*rtResponse, error) {
	r := *req // shallow copy: don't mutate the caller's request
	r.OutputMode = mode
	body, err := c.translator.SerializeRequest(&r)
	if err != nil {
		return nil, fmt.Errorf("relay client: serialize request: %w", err)
	}
	return c.transport.roundTrip(ctx, c, body)
}

// Event is one canonical stream event: its name (a v1.Event* constant) and the
// raw JSON payload. Decode Data into the matching v1 event struct as needed.
type Event struct {
	Type string
	Data []byte
}

// Stream iterates canonical events. For a relay target the upstream stream is
// already canonical (toCanon is nil, frames pass through); for a vendor target
// toCanon converts each vendor SSE frame to canonical.
type Stream struct {
	body    io.ReadCloser
	sc      *bufio.Scanner
	toCanon func([]byte) ([]byte, error)
	pending [][]byte // canonical frames produced from one upstream frame, not yet returned

	// Reasoning span, tracked as the caller drains via Recv. Offsets from
	// start (GenerateStream call), mirroring relay's server-side timing so
	// a direct (non-relay) consumer gets the same data quality.
	start          time.Time
	reasoningStart time.Duration
	reasoningEnd   time.Duration
	reasoningSeen  bool
}

// Recv returns the next canonical event, or io.EOF at end.
func (s *Stream) Recv() (*Event, error) {
	for {
		if len(s.pending) > 0 {
			frame := s.pending[0]
			s.pending = s.pending[1:]
			if event, data, ok := v1.ParseSSEChunk(frame); ok {
				if v1.IsReasoningEvent(event, data) {
					now := time.Since(s.start)
					if !s.reasoningSeen {
						s.reasoningStart = now
						s.reasoningSeen = true
					}
					s.reasoningEnd = now
				}
				return &Event{Type: event, Data: append([]byte(nil), data...)}, nil
			}
			continue
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		raw := append(append([]byte(nil), s.sc.Bytes()...), '\n', '\n')
		if s.toCanon == nil {
			s.pending = [][]byte{raw}
			continue
		}
		out, err := s.toCanon(raw)
		if err != nil {
			return nil, err
		}
		s.pending = splitFrames(out)
	}
}

// Close releases the underlying response body.
func (s *Stream) Close() error { return s.body.Close() }

// ReasoningSpan reports the reasoning span observed so far: offsets from the
// GenerateStream call to the first and last reasoning frames, and ok=false if
// the stream carried no reasoning. Call it after draining the stream for the
// full span; mid-stream it reflects reasoning seen up to the last Recv.
func (s *Stream) ReasoningSpan() (start, end time.Duration, ok bool) {
	return s.reasoningStart, s.reasoningEnd, s.reasoningSeen
}

// APIError is a non-2xx response from the target.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Raw        []byte
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("relay client: %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("relay client: %d: %s", e.StatusCode, string(e.Raw))
}

// parseAPIError best-effort-extracts a code/message from the common
// {"error":{...}} envelope (relay, OpenAI, Anthropic all use it).
func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{StatusCode: status, Raw: body}
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wire) == nil {
		e.Code = wire.Error.Code
		if e.Code == "" {
			e.Code = wire.Error.Type // Anthropic uses error.type
		}
		e.Message = wire.Error.Message
	}
	return e
}

// splitSSEFrames is a bufio.SplitFunc yielding one SSE frame per token.
func splitSSEFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// splitFrames splits concatenated SSE bytes into individual frames.
func splitFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			if len(bytes.TrimSpace(b)) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		if len(bytes.TrimSpace(b[:i])) > 0 {
			frames = append(frames, b[:i+2])
		}
		b = b[i+2:]
	}
	return frames
}
