package ollama

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/wyolet/relay/pkg/transport"
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

// ChatCompletions forwards an OpenAI-shaped chat completion to Ollama and
// emits response chunks as *transport.Messages on out. The first message
// carries X-Relay-Status and Content-Type headers; subsequent messages carry
// body chunks; the final message carries X-Relay-Final="true".
func (c *Client) ChatCompletions(ctx context.Context, body []byte, out chan<- *transport.Message) error {
	defer close(out)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		out <- errorMessage(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		out <- errorMessage(err)
		return fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()

	// Header-only first message: status + content-type.
	out <- &transport.Message{
		Headers: map[string]string{
			"X-Relay-Status": strconv.Itoa(resp.StatusCode),
			"Content-Type":   resp.Header.Get("Content-Type"),
		},
	}

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
	return []byte(fmt.Sprintf(
		`{"error":{"message":"upstream: %s","type":"upstream_error","code":"upstream_unavailable"}}`,
		jsonEscapeString(err.Error()),
	))
}

func jsonEscapeString(s string) string {
	b, _ := marshalString(s)
	return string(b[1 : len(b)-1])
}

func marshalString(s string) ([]byte, error) {
	buf := make([]byte, 0, len(s)+2)
	buf = append(buf, '"')
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
	buf = append(buf, '"')
	return buf, nil
}
