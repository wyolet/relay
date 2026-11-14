package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func sampleReq() *v1.Request {
	return &v1.Request{
		Model:        v1.ModelRefs{"some-model"},
		Instructions: "be concise",
		CacheConfig:  &v1.CacheConfig{Instructions: true},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
}

// sampleWire is sampleReq on the canonical wire with the given output mode.
func sampleWire(mode string) string {
	return `{"model":"some-model","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"cache_config":{"instructions":true},"output_mode":"` + mode + `"}`
}

const (
	canonicalReply = `{"id":"resp_1","object":"response","created_at":0,"model":"m","status":"completed","finish_reason":"stop","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input":1000,"output":500}}`
	ccReply        = `{"id":"cc1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`
)

var canonicalStream = strings.Join([]string{
	"event: generation.created\ndata: {\"id\":\"resp_1\",\"model\":\"m\"}\n\n",
	"event: item.delta\ndata: {\"item_id\":\"msg_1\",\"index\":0,\"kind\":\"text\",\"delta\":\"hi\"}\n\n",
	"event: generation.completed\ndata: {\"id\":\"resp_1\",\"status\":\"completed\",\"finish_reason\":\"stop\"}\n\n",
}, "")

// sentRequest is what a recorder saw of one request.
type sentRequest struct {
	Method, Path, Query, Body string
	Header                    http.Header
}

// recorder answers every request with one status and body and keeps what it received.
type recorder struct {
	mu   sync.Mutex
	reqs []sentRequest
	srv  *httptest.Server
}

func newRecorder(t *testing.T, status int, reply string) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, sentRequest{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Body: string(body), Header: req.Header.Clone()})
		r.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// host is the recorder's host:port, as APIError.Host reports it.
func (r *recorder) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

// only returns the single request received, failing unless there was exactly one.
func (r *recorder) only(t *testing.T) sentRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(r.reqs))
	}
	return r.reqs[0]
}

// drain opens a stream for sampleReq and returns every event up to io.EOF.
func drain(t *testing.T, c *Client) []Event {
	t.Helper()
	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	return recvAll(t, stream)
}

func recvAll(t *testing.T, s *Stream) []Event {
	t.Helper()
	var got []Event
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, *ev)
	}
}

const testCatalogJSON = `{
  "version": "test@v1",
  "generatedAt": "2026-05-28T12:00:00Z",
  "hosts": [
    {
      "name": "openai-direct",
      "baseURL": "https://api.openai.com",
      "models": [
        {
          "model": "gpt-4o",
          "providers": ["openai"],
          "adapter": "openai",
          "upstream": "gpt-4o-2024-08-06",
          "pricing": [
            {"meter":"tokens.input","unit":"per_million","amount":2.5,"aboveTokens":0},
            {"meter":"tokens.output","unit":"per_million","amount":10,"aboveTokens":0}
          ]
        }
      ]
    },
    {
      "name": "ollama-local",
      "baseURL": "http://127.0.0.1:11434",
      "models": [
        {
          "model": "llama3",
          "providers": ["meta"],
          "adapter": "openai",
          "upstream": "llama3"
        }
      ]
    }
  ]
}`

func testCatalog(t *testing.T) *catalog.IndexedCatalog {
	t.Helper()
	cat, err := catalog.LoadBytes([]byte(testCatalogJSON))
	if err != nil {
		t.Fatalf("load test catalog: %v", err)
	}
	return cat
}

// catalogClient resolves ref in the test catalog and points the client at baseURL.
func catalogClient(t *testing.T, ref, baseURL string, opts ...TargetOption) *Client {
	t.Helper()
	c, err := ForFrom(testCatalog(t), ref, "sk-provider", append([]TargetOption{WithBaseURL(baseURL)}, opts...)...)
	if err != nil {
		t.Fatalf("ForFrom(%q): %v", ref, err)
	}
	return c
}
