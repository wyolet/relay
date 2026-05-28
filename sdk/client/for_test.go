package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

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

func forFromCatalog(t *testing.T, cat *catalog.IndexedCatalog, ref, apiKey string, opts ...TargetOption) *Client {
	t.Helper()
	binding, host, err := cat.Resolve(ref)
	if err != nil {
		t.Fatalf("resolve %q: %v", ref, err)
	}
	target, err := targetFromBinding(binding, host)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	for _, o := range opts {
		if err := o(&target); err != nil {
			t.Fatalf("target option: %v", err)
		}
	}
	return target.client(apiKey)
}

func TestFor_DirectOpenAI(t *testing.T) {
	cat := testCatalog(t)
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotModel, _ = body["model"].(string)
		_, _ = w.Write([]byte(`{"id":"cc1","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`))
	}))
	defer srv.Close()

	c := forFromCatalog(t, cat, "gpt-4o@openai-direct", "sk-test", WithBaseURL(srv.URL))
	resp, err := c.Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path: %q", gotPath)
	}
	if gotModel != "gpt-4o-2024-08-06" {
		t.Errorf("upstream model: %q", gotModel)
	}
	cost, ok := resp.Cost()
	if !ok {
		t.Fatal("expected priced response")
	}
	want := 2.5*0.001 + 10*0.0005
	if cost != want {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestFor_UnpricedHost(t *testing.T) {
	cat := testCatalog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"cc1","object":"chat.completion","model":"llama3",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	c := forFromCatalog(t, cat, "llama3@ollama-local", "", WithBaseURL(srv.URL))
	resp, err := c.Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Cost(); ok {
		t.Fatal("expected unpriced response")
	}
}

func TestRelay_CostUnpriced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := &v1.Response{
			ID: "r1", Object: "response", Status: v1.StatusCompleted,
			Usage: usage.Tokens{"input": 1000, "output": 500},
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	resp, err := Relay(srv.URL, "rk").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Cost(); ok {
		t.Fatal("relay target should not expose SDK-side cost")
	}
}

func TestFor_StreamCost(t *testing.T) {
	cat := testCatalog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunks := []string{
			`data: {"id":"cc1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
			`data: {"id":"cc1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":100000}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n\n"))
		}
	}))
	defer srv.Close()

	c := forFromCatalog(t, cat, "gpt-4o@openai-direct", "sk", WithBaseURL(srv.URL))
	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	cost, ok := stream.Cost()
	if !ok {
		t.Fatal("expected stream cost")
	}
	want := 2.5 + 1.0
	if cost != want {
		t.Fatalf("stream cost = %v, want %v", cost, want)
	}
}

func TestWithAdapterName(t *testing.T) {
	cat := testCatalog(t)
	target, err := targetFromBinding(
		catalog.Binding{Model: "gpt-4o", Adapter: "openai", Upstream: "gpt-4o"},
		catalog.Host{Name: "h", BaseURL: "http://example.com"},
	)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := target.withAdapterName("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if updated.adapter.path != "/v1/messages" {
		t.Fatalf("adapter path = %q", updated.adapter.path)
	}
	_ = cat
}
