package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/wyolet/relay/internal/provider"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/transport"
)

const defaultBaseURL = "https://api.anthropic.com"
const anthropicVersion = "2023-06-01"

// Client is an Anthropic provider client. It satisfies provider.MessagesOutbound.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ provider.MessagesOutbound = (*Client)(nil)

// New returns an Anthropic client. If baseURL is empty, defaultBaseURL is used.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{},
	}
}

// Messages forwards an Anthropic-shaped messages request and emits response
// chunks as *transport.Messages on out. The first message carries X-Relay-Status
// and Content-Type headers; subsequent messages carry body chunks; the final
// message carries X-Relay-Final="true".
func (c *Client) Messages(ctx context.Context, body []byte, secret string, out chan<- *transport.Message) error {
	defer close(out)
	start := time.Now()
	statusCode := 0
	defer func() {
		provider.MetricUpstreamDuration.WithLabelValues("anthropic", provider.StatusClass(statusCode)).Observe(time.Since(start).Seconds())
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		out <- errorMessage(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if secret != "" {
		req.Header.Set("x-api-key", secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		out <- errorMessage(err)
		return fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode

	firstHeaders := map[string]string{
		"X-Relay-Status": strconv.Itoa(resp.StatusCode),
		"Content-Type":   resp.Header.Get("Content-Type"),
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		firstHeaders["Retry-After"] = ra
	}
	out <- &transport.Message{Headers: firstHeaders}

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return ctx.Err()
		default:
		}

		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			out <- &transport.Message{Body: chunk}
		}
		if rerr == io.EOF {
			out <- &transport.Message{Headers: map[string]string{"X-Relay-Final": "true"}}
			return nil
		}
		if rerr != nil {
			out <- &transport.Message{
				Body:    errorEnvelope(rerr),
				Headers: map[string]string{"X-Relay-Final": "true"},
			}
			return rerr
		}
	}
}

func errorMessage(err error) *transport.Message {
	return &transport.Message{
		Body: errorEnvelope(err),
		Headers: map[string]string{
			"X-Relay-Status": "502",
			"Content-Type":   "application/json",
			"X-Relay-Final":  "true",
		},
	}
}

func errorEnvelope(err error) []byte {
	msg := jsonEscapeString(httpheader.SafeUpstreamError("anthropic", err))
	return []byte(`{"type":"error","error":{"type":"upstream_error","message":"` + msg + `"}}`)
}

func jsonEscapeString(s string) string {
	buf := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			buf = append(buf, c)
		}
	}
	return string(buf)
}
