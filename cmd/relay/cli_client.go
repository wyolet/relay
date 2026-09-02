// cli_client.go is the shared HTTP plumbing for the control-plane
// subcommands (apply, export, token). Plain net/http against the same
// endpoints an operator would curl — no generated client, no new module.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultControlURL matches the control listener's default port.
const DefaultControlURL = "http://localhost:5103"

type controlClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newControlClient(url, token string) *controlClient {
	if url == "" {
		url = os.Getenv("RELAY_URL")
	}
	if url == "" {
		url = DefaultControlURL
	}
	if token == "" {
		token = os.Getenv("RELAY_ADMIN_TOKEN")
	}
	return &controlClient{
		baseURL: strings.TrimSuffix(url, "/"),
		token:   token,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// httpError carries a non-2xx response so callers can map it to exit 2.
type httpError struct {
	Status int
	Body   []byte
}

func (e *httpError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, strings.TrimSpace(string(e.Body)))
}

func (c *controlClient) do(method, path, contentType string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, &httpError{Status: resp.StatusCode, Body: out}
	}
	return out, nil
}

// runCLI runs a control-plane subcommand and exits with its chosen code.
// Subcommands signal a non-default code with *exitError; anything else is 1.
func runCLI(name string, fn func([]string) error, args []string) {
	err := fn(args)
	if err == nil {
		return
	}
	code := 1
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
	os.Exit(code)
}
