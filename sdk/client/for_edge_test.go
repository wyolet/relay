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

// White-box: build a Target directly (unexported fields) to test the
// catalog-resolved call path without depending on the live embedded catalog.

func bindingTarget(srvURL, upstream string, pricing []catalog.Rate) Target {
	return Target{
		baseURL:  srvURL,
		adapter:  adapters["openai"],
		upstream: upstream,
		binding:  catalog.Binding{MetadataName: "gpt-4o", Adapter: "openai", Name: upstream, Pricing: pricing},
	}
}

// The upstream wire name from the catalog must replace the caller's model ref
// in the outgoing body, and the caller's request must NOT be mutated.
func TestFor_UpstreamModelSubstitution(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var wire map[string]any
		_ = json.Unmarshal(b, &wire)
		gotModel, _ = wire["model"].(string)
		resp := &v1.Response{ID: "r1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}
		bb, _ := json.Marshal(resp)
		_, _ = w.Write(bb)
	}))
	defer srv.Close()

	req := &v1.Request{Model: v1.ModelRefs{"gpt-4o"}}
	c := bindingTarget(srv.URL, "gpt-4o-2024-08-06", nil).client("k")
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if gotModel != "gpt-4o-2024-08-06" {
		t.Errorf("upstream model not sent: got %q", gotModel)
	}
	if req.Model[0] != "gpt-4o" {
		t.Errorf("caller request mutated: req.Model[0]=%q", req.Model[0])
	}
}

// When the caller sends a request with NO model, the catalog-resolved upstream
// name is injected — the caller already picked the model via the ref.
func TestFor_EmptyCallerModel_UpstreamInjected(t *testing.T) {
	var gotModel string
	var hadModel bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var wire map[string]any
		_ = json.Unmarshal(b, &wire)
		gotModel, hadModel = wire["model"].(string)
		resp := &v1.Response{ID: "r1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}
		bb, _ := json.Marshal(resp)
		_, _ = w.Write(bb)
	}))
	defer srv.Close()

	req := &v1.Request{} // no model
	c := bindingTarget(srv.URL, "gpt-4o-2024-08-06", nil).client("k")
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !hadModel || gotModel != "gpt-4o-2024-08-06" {
		t.Fatalf("For() should inject upstream %q, sent model=%q (present=%v)",
			"gpt-4o-2024-08-06", gotModel, hadModel)
	}
}

// WithClient threads client Options (here an extra header) through a
// catalog-resolved client.
func TestWithClient_PassesOptions(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Test")
		resp := &v1.Response{ID: "r1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop}
		bb, _ := json.Marshal(resp)
		_, _ = w.Write(bb)
	}))
	defer srv.Close()

	tgt := bindingTarget(srv.URL, "gpt-4o-2024-08-06", nil)
	tgt.clientOpts = []Option{WithHeader("X-Test", "yes")}
	c := tgt.client("k")
	if _, err := c.Generate(context.Background(), &v1.Request{Model: v1.ModelRefs{"gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "yes" {
		t.Errorf("WithClient header not applied: %q", gotHeader)
	}
}

// Cost is false for an unpriced binding and a real number for a priced one.
func TestResponse_Cost_PricedAndUnpriced(t *testing.T) {
	resp := &v1.Response{Usage: usageWith(1_000_000, 0)}

	unpriced := &Response{Response: resp, priced: false}
	if c, ok := unpriced.Cost(); ok || c != 0 {
		t.Errorf("unpriced: got (%v,%v)", c, ok)
	}

	priced := &Response{
		Response: resp,
		priced:   true,
		binding: catalog.Binding{Pricing: []catalog.Rate{
			{Meter: "tokens.input", Unit: "per_million", Amount: 2.5},
		}},
	}
	c, ok := priced.Cost()
	if !ok || c != 2.5 {
		t.Errorf("priced: got (%v,%v), want (2.5,true)", c, ok)
	}
}

func usageWith(input, output int64) usage.Tokens {
	return usage.Tokens{"input": input, "output": output}
}
