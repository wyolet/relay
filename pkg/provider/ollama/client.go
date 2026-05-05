package ollama

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{},
	}
}

// ChatCompletions forwards an OpenAI-shaped chat completion to Ollama
// (which exposes the OpenAI-compat endpoint at /v1/chat/completions),
// streams the response back to w, copies upstream status and
// content-type, and flushes after each chunk so SSE works.
func (c *Client) ChatCompletions(ctx context.Context, body []byte, w http.ResponseWriter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	return streamCopy(w, resp.Body)
}

func streamCopy(w http.ResponseWriter, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
