// Package client is a thin, dependency-free Go client for a Relay server's
// canonical endpoint (POST /v1/generate). It speaks pkg/relay/v1 types
// directly: callers build a *v1.Request (including provider-neutral knobs like
// CacheConfig), and get back a *v1.Response or a stream of canonical events,
// regardless of which upstream vendor the relay routes to.
//
// It imports only the standard library and pkg/relay/v1 (pure stdlib), so
// depending on it pulls nothing of relay's server-side dependency graph.
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

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

const generatePath = "/v1/generate"

// Client targets one Relay base URL with one relay key.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client (e.g. for custom timeouts
// or transports). Streaming requires a client without a short overall timeout.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a Client for baseURL (e.g. "https://relay.example.com"),
// authenticating with the given relay key as a bearer token.
func New(baseURL, relayKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  relayKey,
		http:    http.DefaultClient,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Generate runs a non-streaming canonical generation and returns the response.
// The request's OutputMode is forced to sync; the caller's value is untouched.
func (c *Client) Generate(ctx context.Context, req *v1.Request) (*v1.Response, error) {
	httpResp, err := c.post(ctx, req, v1.OutputModeSync)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("relay client: read body: %w", err)
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, parseAPIError(httpResp.StatusCode, body)
	}
	return v1.ParseResponse(body)
}

// GenerateStream runs a streaming canonical generation. The returned *Stream
// yields canonical events until io.EOF; the caller must Close it. OutputMode is
// forced to stream.
func (c *Client) GenerateStream(ctx context.Context, req *v1.Request) (*Stream, error) {
	httpResp, err := c.post(ctx, req, v1.OutputModeStream)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		return nil, parseAPIError(httpResp.StatusCode, body)
	}
	sc := bufio.NewScanner(httpResp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(splitSSEFrames)
	return &Stream{body: httpResp.Body, sc: sc}, nil
}

func (c *Client) post(ctx context.Context, req *v1.Request, mode string) (*http.Response, error) {
	r := *req // shallow copy so we don't mutate the caller's request
	r.OutputMode = mode
	body, err := json.Marshal(&r)
	if err != nil {
		return nil, fmt.Errorf("relay client: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+generatePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return c.http.Do(httpReq)
}

// Event is one canonical stream event: its name (a v1.Event* constant) and the
// raw JSON payload. Decode Data into the matching v1 event struct
// (v1.GenerationCompletedEvent, v1.ItemDeltaEvent, …) as needed.
type Event struct {
	Type string
	Data []byte
}

// Stream is an iterator over a canonical event stream.
type Stream struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

// Recv returns the next event, or io.EOF when the stream ends. Frames without
// a data payload (e.g. SSE comments) are skipped.
func (s *Stream) Recv() (*Event, error) {
	for s.sc.Scan() {
		event, data, ok := v1.ParseSSEChunk(s.sc.Bytes())
		if !ok {
			continue
		}
		return &Event{Type: event, Data: append([]byte(nil), data...)}, nil
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the underlying response body.
func (s *Stream) Close() error { return s.body.Close() }

// APIError is a non-2xx response from the relay server.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Raw        []byte
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("relay: %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("relay: %d: %s", e.StatusCode, string(e.Raw))
}

func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{StatusCode: status, Raw: body}
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wire) == nil {
		e.Code = wire.Error.Code
		e.Message = wire.Error.Message
	}
	return e
}

// splitSSEFrames is a bufio.SplitFunc that yields one SSE frame per token,
// splitting on the blank-line separator.
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
